package osm

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jonas-p/go-shp"
	"github.com/pocketbase/pocketbase/core"

	"github.com/sellography/geocoder-pb/internal/config"
	"github.com/sellography/geocoder-pb/internal/geocoder"
	"github.com/sellography/geocoder-pb/internal/tiger"
)

// Scheduler handles periodic OSM data refresh.
type Scheduler struct {
	cfg *config.Config
	geo *geocoder.Geocoder
}

// NewScheduler creates a new OSM update scheduler.
func NewScheduler(cfg *config.Config, geo *geocoder.Geocoder) *Scheduler {
	return &Scheduler{cfg: cfg, geo: geo}
}

// Start registers and starts the cron scheduler on the provided PocketBase app.
func (s *Scheduler) Start(ctx context.Context, app core.App) error {
	if err := app.Cron().Add("osm-refresh", s.cfg.UpdateCron, func() {
		if err := s.Refresh(ctx); err != nil {
			app.Logger().Error(fmt.Sprintf("osm refresh failed: %v", err))
		}
	}); err != nil {
		return fmt.Errorf("invalid update cron: %w", err)
	}

	app.Cron().Start()
	return nil
}

// EnsureIndexed downloads and indexes OSM data if needed, resuming interrupted
// work automatically:
//
//   - places table empty          -> full download + import
//   - import crashed mid-parse    -> re-import (upserts are idempotent)
//   - FTS rebuild crashed mid-way -> resume from the last committed chunk
//   - legacy fully-populated DBs  -> completion markers are backfilled, no re-import
//
// Completion is tracked via markers in _import_state because the previous
// heuristic ("places table has rows -> skip import") left servers with a
// permanently partial index after a crash.
func (s *Scheduler) EnsureIndexed(ctx context.Context, app core.App) error {
	// Check if the places table has any data at all.
	hasPlaces, err := s.geo.HasPlaces(ctx)
	if err != nil {
		return fmt.Errorf("failed to check places table: %w", err)
	}

	if !hasPlaces {
		// Fresh deployment - download and import OSM data.
		app.Logger().Info("places table is empty; downloading and indexing OSM data...")
		return s.Refresh(ctx)
	}

	placesDone, err := s.geo.GetImportState(ctx, geocoder.StateOSMPlacesDone)
	if err != nil {
		return fmt.Errorf("failed to check OSM import state: %w", err)
	}
	ftsEmpty, err := s.geo.NeedsFTSRebuild(ctx)
	if err != nil {
		return fmt.Errorf("failed to check FTS rebuild status: %w", err)
	}

	if placesDone != "done" {
		if ftsEmpty {
			// Rows exist but the import never finished (crash mid-parse):
			// a fully populated FTS means the import must have completed,
			// an empty one means it didn't. Re-import; upserts are idempotent.
			app.Logger().Info("detected interrupted OSM import; resuming (existing rows are upserted, not duplicated)...")
			return s.Refresh(ctx)
		}
		// Rows exist and the FTS index is populated, but there are no
		// completion markers: this is a database created before import-state
		// tracking existed. Backfill the markers instead of re-importing.
		app.Logger().Info("existing index detected; marking import state as complete")
		if err := s.geo.SetImportState(ctx, geocoder.StateOSMPlacesDone, "done"); err != nil {
			app.Logger().Warn("failed to mark OSM import complete", "error", err)
		}
		if err := s.geo.SetImportState(ctx, geocoder.StateOSMFTSDone, "done"); err != nil {
			app.Logger().Warn("failed to mark OSM FTS rebuild complete", "error", err)
		}
	}

	// Places import is complete. Now ensure the FTS index is fully built.
	// RebuildFTS resumes from the last committed chunk if a previous rebuild
	// was interrupted.
	ftsDone, err := s.geo.GetImportState(ctx, geocoder.StateOSMFTSDone)
	if err != nil {
		return fmt.Errorf("failed to check OSM FTS state: %w", err)
	}
	ftsOffset, err := s.geo.GetImportState(ctx, geocoder.StateOSMFTSOffset)
	if err != nil {
		return fmt.Errorf("failed to check OSM FTS rebuild progress: %w", err)
	}

	switch {
	case ftsOffset != "":
		// A previous chunked rebuild was interrupted - resume it.
		app.Logger().Info("resuming interrupted FTS index rebuild...")
		if err := s.geo.RebuildFTS(ctx); err != nil {
			return fmt.Errorf("FTS rebuild failed: %w", err)
		}
		s.geo.Checkpoint(ctx)
	case ftsEmpty:
		app.Logger().Info("FTS index is empty; rebuilding in background...")
		if err := s.geo.RebuildFTS(ctx); err != nil {
			return fmt.Errorf("FTS rebuild failed: %w", err)
		}
		s.geo.Checkpoint(ctx)
	case ftsDone != "done":
		// FTS has rows but no completion marker (legacy DB) - mark it done.
		if err := s.geo.SetImportState(ctx, geocoder.StateOSMFTSDone, "done"); err != nil {
			app.Logger().Warn("failed to mark OSM FTS rebuild complete", "error", err)
		}
	}

	// Ensure the ZIP cache exists for TIGER lookups.
	s.ensureZipCache(ctx, app)

	// Check if a full re-import was requested.
	if s.cfg.ForceReindex {
		app.Logger().Info("force reindex enabled; re-importing OSM data...")
		return s.Refresh(ctx)
	}

	// Everything is already populated.
	app.Logger().Info("geocoder index already populated")
	return nil
}

// ensureZipCache builds the zip_city_state cache table if it doesn't exist.
// This table is needed for fast TIGER address interpolation lookups.
func (s *Scheduler) ensureZipCache(ctx context.Context, app core.App) {
	hasCache, err := s.geo.HasZipCache(ctx)
	if err != nil {
		app.Logger().Error("failed to check zip cache", "error", err)
		return
	}
	if hasCache {
		app.Logger().Info("zip city/state cache already populated")
		return
	}
	app.Logger().Info("building zip city/state cache...")
	if err := s.geo.RebuildZipCache(ctx); err != nil {
		app.Logger().Error("zip cache rebuild failed", "error", err)
	} else {
		app.Logger().Info("zip city/state cache build complete")
	}
}

// EnsureTigerIndexed imports TIGER/Line address range data for the configured
// counties. ImportTiger skips counties already marked as imported, so this is
// cheap on every startup and — importantly — picks up counties that are
// missing (e.g. from an interrupted earlier import) without a full re-import.
// Previously this returned early whenever the table had any rows, which made
// per-county resume markers unreachable and missing counties permanent.
func (s *Scheduler) EnsureTigerIndexed(ctx context.Context, app core.App) error {
	if !s.cfg.TIGERAllCounties && len(s.cfg.TIGERCounties) == 0 {
		return nil
	}

	// If forcing a re-import, clear the progress markers so all counties are re-processed.
	if s.cfg.TIGERForceReimport {
		if err := s.geo.ClearTigerImportState(ctx); err != nil {
			app.Logger().Warn("failed to clear TIGER import state", "error", err)
		}
	}

	if s.cfg.TIGERAllCounties {
		app.Logger().Info("checking TIGER/Line address data for all US counties")
	} else {
		app.Logger().Info("checking TIGER/Line address data", "counties", s.cfg.TIGERCounties)
	}
	if err := s.ImportTiger(ctx, app); err != nil {
		return fmt.Errorf("TIGER import failed: %w", err)
	}
	return nil
}

// ImportTiger downloads and imports TIGER/Line ADDRFEAT shapefiles for all configured counties.
// If TIGERAllCounties is enabled, it fetches the full list of US county FIPS codes
// from the Census Bureau and imports all ~3,200 counties.
func (s *Scheduler) ImportTiger(ctx context.Context, app core.App) error {
	if err := os.MkdirAll(s.cfg.OSMDataPath, 0o755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	tigerDir := filepath.Join(s.cfg.OSMDataPath, "tiger")
	if err := os.MkdirAll(tigerDir, 0o755); err != nil {
		return fmt.Errorf("failed to create tiger data directory: %w", err)
	}

	// Determine the list of county FIPS codes to import.
	var counties []string
	if s.cfg.TIGERAllCounties {
		app.Logger().Info("fetching full list of US county FIPS codes from Census Bureau...")
		allCounties, err := s.fetchAllCountyFIPS(ctx, app)
		if err != nil {
			return fmt.Errorf("failed to fetch county FIPS list: %w", err)
		}
		counties = allCounties
		app.Logger().Info(fmt.Sprintf("found %d US counties to import", len(counties)))
	} else {
		counties = s.cfg.TIGERCounties
	}

	// Reclaim disk from files left behind by earlier versions (which kept
	// every downloaded ZIP and extracted shapefile forever).
	if !s.cfg.TIGERKeepData {
		s.cleanupTigerData(ctx, tigerDir)
	}

	importedCount, doneCount, failedCount := 0, 0, 0
	importedAny := false

	for idx, fips := range counties {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip counties that have already been imported (crash recovery).
		// This allows resuming an interrupted TIGER import without
		// re-processing thousands of completed counties.
		alreadyDone, err := s.geo.IsTigerCountyImported(ctx, fips)
		if err != nil {
			log.Printf("warning: failed to check import state for county %s: %v", fips, err)
		} else if alreadyDone {
			doneCount++
			continue
		}

		log.Printf("TIGER county %d/%d: %s", idx+1, len(counties), fips)

		zipPath := filepath.Join(tigerDir, fmt.Sprintf("tl_%s_%s_addrfeat.zip", s.cfg.TIGERYear, fips))
		extractDir := filepath.Join(tigerDir, fips)

		// Import the county, retrying once with a fresh download if the local
		// files are corrupt (e.g. a truncated ZIP from a previously interrupted
		// download). Corrupt files are deleted, so a failed county also
		// self-heals on the next startup instead of being skipped forever.
		const maxCountyAttempts = 2
		processed := false
		importedRows := 0
		for attempt := 1; attempt <= maxCountyAttempts && !processed; attempt++ {
			// Download the ZIP if it doesn't exist.
			if _, err := os.Stat(zipPath); os.IsNotExist(err) {
				url := fmt.Sprintf("https://www2.census.gov/geo/tiger/TIGER%s/ADDRFEAT/tl_%s_%s_addrfeat.zip",
					s.cfg.TIGERYear, s.cfg.TIGERYear, fips)
				log.Printf("downloading TIGER/Line data from %s...", url)
				if err := downloadFile(ctx, url, zipPath); err != nil {
					log.Printf("failed to download TIGER data for county %s: %v", fips, err)
					break // No file to work with; retry on the next run.
				}
				log.Printf("TIGER/Line data downloaded for county %s", fips)
			} else {
				log.Printf("using existing TIGER/Line data for county %s", fips)
			}

			// Extract the ZIP if the directory doesn't exist.
			if _, err := os.Stat(extractDir); os.IsNotExist(err) {
				if err := unzipFile(zipPath, extractDir); err != nil {
					log.Printf("failed to unzip TIGER data for county %s (attempt %d/%d): %v", fips, attempt, maxCountyAttempts, err)
					removeCountyFiles(zipPath, extractDir)
					continue
				}
			}

			// Parse and import the shapefile. A directory with no shapefile
			// (partial extraction) yields 0 rows and is treated as corrupt.
			parser := tiger.NewParser(s.geo, s.cfg.ImportBatchSize)
			count, err := parser.ParseDir(ctx, extractDir)
			if err != nil {
				log.Printf("failed to parse TIGER data for county %s (attempt %d/%d): %v", fips, attempt, maxCountyAttempts, err)
				removeCountyFiles(zipPath, extractDir)
				continue
			}
			if count == 0 && attempt < maxCountyAttempts {
				log.Printf("no address ranges found for county %s (attempt %d/%d); re-downloading in case the files are corrupt", fips, attempt, maxCountyAttempts)
				removeCountyFiles(zipPath, extractDir)
				continue
			}
			if count == 0 {
				log.Printf("warning: no address ranges found for county %s after %d attempts; marking it done anyway to avoid re-downloading every run", fips, maxCountyAttempts)
			}
			importedRows = count
			processed = true
		}

		if !processed {
			failedCount++
			log.Printf("skipping county %s for now; it will be retried on the next run", fips)
			continue
		}
		importedCount++
		if importedRows > 0 {
			importedAny = true
		}

		// Mark this county as imported so we can skip it on resume.
		if err := s.geo.MarkTigerCountyImported(ctx, fips); err != nil {
			log.Printf("warning: failed to mark county %s as imported: %v", fips, err)
		}

		// Free the disk space: the full US TIGER dataset is ~100GB unpacked,
		// which does not fit on small VPS disks. The county marker above means
		// the files are not needed again.
		if !s.cfg.TIGERKeepData {
			removeCountyFiles(zipPath, extractDir)
		}
	}

	log.Printf("TIGER county check complete: %d imported, %d already done, %d failed",
		importedCount, doneCount, failedCount)

	// When new rows were added, the TIGER FTS index is stale: clear the done
	// marker so it gets rebuilt below and the new rows become searchable.
	if importedAny {
		if err := s.geo.DeleteImportState(ctx, geocoder.StateTigerFTSDone); err != nil {
			log.Printf("warning: failed to clear TIGER FTS state: %v", err)
		}
	}

	// Rebuild the TIGER FTS index if needed (chunked + resumable). On
	// steady-state startups (nothing imported, rebuild already complete, no
	// resume pending) this is skipped entirely.
	ftsDone, _ := s.geo.GetImportState(ctx, geocoder.StateTigerFTSDone)
	ftsOffset, _ := s.geo.GetImportState(ctx, geocoder.StateTigerFTSOffset)
	if ftsDone != "done" || ftsOffset != "" {
		if err := s.geo.RebuildTigerFTS(ctx); err != nil {
			app.Logger().Warn("failed to rebuild TIGER FTS index", "error", err)
		}
	}

	if importedAny {
		// Flush the WAL back into the main DB file to reclaim disk space.
		s.geo.Checkpoint(ctx)

		// Rebuild the ZIP cache since TIGER lookups depend on it. Skip if there
		// is no OSM data yet (the OSM side rebuilds it once places exist).
		if hasPlaces, err := s.geo.HasPlaces(ctx); err == nil && hasPlaces {
			if err := s.geo.RebuildZipCache(ctx); err != nil {
				app.Logger().Warn("failed to rebuild zip cache", "error", err)
			}
		}
	}

	return nil
}

// fetchAllCountyFIPS fetches the complete list of US county FIPS codes from the
// Census Bureau's TIGER/Line county shapefile. This is used when TIGERAllCounties
// is enabled to import address data for every US county.
func (s *Scheduler) fetchAllCountyFIPS(ctx context.Context, app core.App) ([]string, error) {
	// Download the county shapefile ZIP from the Census Bureau.
	zipPath := filepath.Join(s.cfg.OSMDataPath, "tiger", fmt.Sprintf("tl_%s_us_county.zip", s.cfg.TIGERYear))
	if _, err := os.Stat(zipPath); os.IsNotExist(err) {
		url := fmt.Sprintf("https://www2.census.gov/geo/tiger/TIGER%s/COUNTY/tl_%s_us_county.zip",
			s.cfg.TIGERYear, s.cfg.TIGERYear)
		log.Printf("downloading US county FIPS reference from %s...", url)
		if err := downloadFile(ctx, url, zipPath); err != nil {
			return nil, fmt.Errorf("failed to download county FIPS reference: %w", err)
		}
	}

	// Extract the ZIP.
	extractDir := filepath.Join(s.cfg.OSMDataPath, "tiger", "county_ref")
	if _, err := os.Stat(extractDir); os.IsNotExist(err) {
		if err := unzipFile(zipPath, extractDir); err != nil {
			return nil, fmt.Errorf("failed to unzip county FIPS reference: %w", err)
		}
	}

	// Parse the DBF file to extract FIPS codes.
	// The county shapefile has COUNTYFP field at index 1 and STATEFP at index 0.
	// The full FIPS code is STATEFP + COUNTYFP (5 digits total).
	dbfPath := filepath.Join(extractDir, fmt.Sprintf("tl_%s_us_county.dbf", s.cfg.TIGERYear))
	fipsCodes, err := parseCountyFIPSFromDBF(dbfPath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse county FIPS codes: %w", err)
	}

	return fipsCodes, nil
}

// parseCountyFIPSFromDBF reads a TIGER county shapefile DBF and extracts all
// 5-digit county FIPS codes (STATEFP + COUNTYFP).
func parseCountyFIPSFromDBF(dbfPath string) ([]string, error) {
	shape, err := shp.Open(dbfPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open county DBF: %w", err)
	}
	defer shape.Close()

	var fipsCodes []string
	for i := 0; shape.Next(); i++ {
		// STATEFP (field 0) + COUNTYFP (field 1) = 5-digit FIPS code
		stateFP := strings.TrimSpace(shape.ReadAttribute(i, 0))
		countyFP := strings.TrimSpace(shape.ReadAttribute(i, 1))
		if stateFP != "" && countyFP != "" {
			fipsCodes = append(fipsCodes, stateFP+countyFP)
		}
	}

	return fipsCodes, nil
}

// Refresh downloads the latest OSM extract and re-indexes places, then rebuilds
// the FTS index as a chunked, resumable operation.
//
// Data safety: This function does NOT clear the existing index before importing.
// Instead, it uses upserts (ON CONFLICT DO UPDATE), so if the import crashes
// mid-way, the database retains all previously-imported records plus any new
// records that were processed before the crash. Completion markers are only
// set after each phase finishes, so an interrupted refresh is automatically
// re-run (rows) or resumed (FTS rebuild) on the next startup.
//
// To force a clean re-import (removing stale records), set FORCE_REINDEX=true
// and manually clear the index before starting.
func (s *Scheduler) Refresh(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.OSMDataPath, 0o755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	path := filepath.Join(s.cfg.OSMDataPath, "us-latest.osm.pbf")

	// Skip download if the file already exists (useful for development).
	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("downloading osm data from %s...", s.cfg.OSMDataURL)
		if err := downloadFile(ctx, s.cfg.OSMDataURL, path); err != nil {
			return fmt.Errorf("failed to download osm data: %w", err)
		}
		log.Printf("osm data downloaded to %s", path)
	} else {
		log.Printf("using existing osm data at %s", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open osm data: %w", err)
	}
	defer f.Close()

	// Note: We intentionally do NOT call ClearIndex() here.
	// Upserts are idempotent (ON CONFLICT DO UPDATE), so re-importing is safe.
	// If the import crashes, existing data is preserved rather than lost.

	// Clear the completion markers up front: if the process dies mid-import,
	// the missing markers tell EnsureIndexed to re-run on the next start
	// instead of mistaking a partial index for a complete one.
	if err := s.geo.DeleteImportState(ctx, geocoder.StateOSMPlacesDone); err != nil {
		log.Printf("warning: failed to clear OSM import state: %v", err)
	}
	if err := s.geo.DeleteImportState(ctx, geocoder.StateOSMFTSDone); err != nil {
		log.Printf("warning: failed to clear OSM FTS state: %v", err)
	}

	parser := NewParser(s.geo, s.cfg.ImportBatchSize, s.cfg.OSMDecoderWorkers)
	if err := parser.Parse(ctx, f); err != nil {
		// Delete the extract so the next startup downloads a fresh copy
		// instead of failing on the same corrupt file forever (e.g. a
		// truncated download left behind by an interrupted earlier run).
		f.Close()
		if rmErr := os.Remove(path); rmErr != nil {
			log.Printf("warning: failed to delete %s after parse failure: %v", path, rmErr)
		} else {
			log.Printf("deleted %s after parse failure; it will be re-downloaded on the next run", path)
		}
		return fmt.Errorf("failed to parse osm data: %w", err)
	}
	f.Close() // Close early so the file can be deleted below if needed.

	if err := s.geo.SetImportState(ctx, geocoder.StateOSMPlacesDone, "done"); err != nil {
		log.Printf("warning: failed to mark OSM import complete: %v", err)
	}

	// Rebuild the FTS index in committed chunks (crash-resumable; a previous
	// interrupted rebuild resumes from its last committed chunk).
	if err := s.geo.RebuildFTS(ctx); err != nil {
		return fmt.Errorf("failed to rebuild FTS index after import: %w", err)
	}

	// Flush the WAL back into the main DB file to reclaim disk space.
	s.geo.Checkpoint(ctx)

	if !s.cfg.OSMKeepData {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("warning: failed to delete OSM extract %s: %v", path, err)
		} else {
			log.Printf("deleted OSM extract %s (OSM_KEEP_DATA=false)", path)
		}
	}

	return nil
}

// tigerCountyZipPattern matches downloaded county archives, e.g. tl_2025_25017_addrfeat.zip.
var tigerCountyZipPattern = regexp.MustCompile(`^tl_\d{4}_(\d{5})_addrfeat\.zip$`)

// removeCountyFiles deletes a county's downloaded ZIP and extracted directory,
// e.g. after a failed unzip/parse so the next attempt downloads a fresh copy.
func removeCountyFiles(zipPath, extractDir string) {
	if err := os.Remove(zipPath); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: failed to delete %s: %v", zipPath, err)
	}
	if err := os.RemoveAll(extractDir); err != nil {
		log.Printf("warning: failed to delete %s: %v", extractDir, err)
	}
}

// cleanupTigerData deletes TIGER ZIPs and extracted directories for counties
// that are already marked as imported. Older versions kept these files
// forever (~100GB for the full US dataset), which exhausts small VPS disks.
func (s *Scheduler) cleanupTigerData(ctx context.Context, tigerDir string) {
	entries, err := os.ReadDir(tigerDir)
	if err != nil {
		return
	}

	removed := 0
	for _, entry := range entries {
		fips := ""
		if m := tigerCountyZipPattern.FindStringSubmatch(entry.Name()); m != nil {
			fips = m[1]
		} else if entry.IsDir() && len(entry.Name()) == 5 && isDigits(entry.Name()) {
			fips = entry.Name()
		} else {
			continue
		}

		done, err := s.geo.IsTigerCountyImported(ctx, fips)
		if err != nil || !done {
			continue
		}

		path := filepath.Join(tigerDir, entry.Name())
		var rmErr error
		if entry.IsDir() {
			rmErr = os.RemoveAll(path)
		} else {
			rmErr = os.Remove(path)
		}
		if rmErr != nil {
			log.Printf("warning: failed to remove stale TIGER data %s: %v", path, rmErr)
		} else {
			removed++
		}
	}

	if removed > 0 {
		log.Printf("removed %d stale TIGER data files/directories for already-imported counties", removed)
	}
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// downloadHTTPClient is used for large dataset downloads. It has connection
// and header timeouts (so a stalled server fails fast) but no overall timeout,
// which would abort legitimate multi-GB downloads on slow links.
var downloadHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	},
}

// downloadFile fetches url to path with retries. The file is written to a
// temporary ".part" file and atomically renamed on success, so an interrupted
// download is never mistaken for a complete cached file on the next run.
func downloadFile(ctx context.Context, url, path string) error {
	const maxAttempts = 3

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = downloadFileOnce(ctx, url, path); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("download attempt %d/%d failed for %s: %v", attempt, maxAttempts, url, err)
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			}
		}
	}
	return fmt.Errorf("download failed after %d attempts: %w", maxAttempts, err)
}

func downloadFileOnce(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := downloadHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	tmpPath := path + ".part"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}

	return os.Rename(tmpPath, path)
}

// unzipFile extracts a ZIP archive to the target directory.
func unzipFile(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer r.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	for _, f := range r.File {
		// Prevent Zip Slip vulnerability.
		fpath := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path in zip: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}

	return nil
}
