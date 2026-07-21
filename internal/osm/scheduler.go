package osm

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

// EnsureIndexed downloads and indexes OSM data if the places table is empty.
// If places already exist but the FTS index is empty, it rebuilds the FTS index.
// Uses fast EXISTS checks instead of COUNT(*) to avoid scanning 54M rows.
func (s *Scheduler) EnsureIndexed(ctx context.Context, app core.App) error {
	// Check if the places table has any data at all.
	hasPlaces, err := s.geo.HasPlaces(ctx)
	if err != nil {
		return fmt.Errorf("failed to check places table: %w", err)
	}

	if !hasPlaces {
		// Fresh deployment — download and import OSM data.
		app.Logger().Info("places table is empty; downloading and indexing OSM data...")
		return s.Refresh(ctx)
	}

	// Places exist — check if the FTS index needs rebuilding.
	needsRebuild, err := s.geo.NeedsFTSRebuild(ctx)
	if err != nil {
		return fmt.Errorf("failed to check FTS rebuild status: %w", err)
	}

	if needsRebuild {
		// Places exist but FTS is empty — rebuild the FTS index.
		app.Logger().Info("FTS index is empty; rebuilding in background...")
		if err := s.geo.RebuildFTS(ctx); err != nil {
			app.Logger().Error("FTS rebuild failed", "error", err)
		} else {
			app.Logger().Info("FTS index rebuild complete")
		}
		// Also rebuild the ZIP cache since places data is available.
		s.ensureZipCache(ctx, app)
		return nil
	}

	// FTS has data — ensure the ZIP cache exists for TIGER lookups.
	s.ensureZipCache(ctx, app)

	// FTS has data — check if we need a full re-import.
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

// EnsureTigerIndexed downloads and imports TIGER/Line address range data
// for the configured counties if the tiger_addr_ranges table is empty.
func (s *Scheduler) EnsureTigerIndexed(ctx context.Context, app core.App) error {
	if !s.cfg.TIGERAllCounties && len(s.cfg.TIGERCounties) == 0 {
		return nil
	}

	// Check if TIGER data already exists.
	hasTiger, err := s.geo.HasTigerAddrRanges(ctx)
	if err != nil {
		return fmt.Errorf("failed to check TIGER addr ranges: %w", err)
	}

	if hasTiger && !s.cfg.TIGERForceReimport {
		app.Logger().Info("TIGER addr ranges already populated")
		return nil
	}

	// If forcing a re-import, clear the progress markers so all counties are re-processed.
	if s.cfg.TIGERForceReimport {
		if err := s.geo.ClearTigerImportState(ctx); err != nil {
			app.Logger().Warn("failed to clear TIGER import state", "error", err)
		}
	}

	if s.cfg.TIGERAllCounties {
		app.Logger().Info("importing TIGER/Line address data for all US counties")
	} else {
		app.Logger().Info("importing TIGER/Line address data", "counties", s.cfg.TIGERCounties)
	}
	if err := s.ImportTiger(ctx, app); err != nil {
		return fmt.Errorf("TIGER import failed: %w", err)
	}
	app.Logger().Info("TIGER/Line import complete")
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
			continue
		}

		log.Printf("TIGER county %d/%d: %s", idx+1, len(counties), fips)

		zipPath := filepath.Join(tigerDir, fmt.Sprintf("tl_%s_%s_addrfeat.zip", s.cfg.TIGERYear, fips))
		extractDir := filepath.Join(tigerDir, fips)

		// Download the ZIP if it doesn't exist.
		if _, err := os.Stat(zipPath); os.IsNotExist(err) {
			url := fmt.Sprintf("https://www2.census.gov/geo/tiger/TIGER%s/ADDRFEAT/tl_%s_%s_addrfeat.zip",
				s.cfg.TIGERYear, s.cfg.TIGERYear, fips)
			log.Printf("downloading TIGER/Line data from %s...", url)
			if err := downloadFile(ctx, url, zipPath); err != nil {
				log.Printf("failed to download TIGER data for county %s: %v", fips, err)
				continue // Skip failed counties instead of aborting the entire import.
			}
			log.Printf("TIGER/Line data downloaded for county %s", fips)
		} else {
			log.Printf("using existing TIGER/Line data for county %s", fips)
		}

		// Extract the ZIP if the directory doesn't exist.
		if _, err := os.Stat(extractDir); os.IsNotExist(err) {
			if err := unzipFile(zipPath, extractDir); err != nil {
				log.Printf("failed to unzip TIGER data for county %s: %v", fips, err)
				continue
			}
		}

		// Parse and import the shapefile.
		parser := tiger.NewParser(s.geo, s.cfg.ImportBatchSize)
		if err := parser.ParseDir(ctx, extractDir); err != nil {
			log.Printf("failed to parse TIGER data for county %s: %v", fips, err)
			continue
		}

		// Mark this county as imported so we can skip it on resume.
		if err := s.geo.MarkTigerCountyImported(ctx, fips); err != nil {
			log.Printf("warning: failed to mark county %s as imported: %v", fips, err)
		}
	}

	// Rebuild the TIGER FTS index after import.
	if err := s.geo.RebuildTigerFTS(ctx); err != nil {
		app.Logger().Warn("failed to rebuild TIGER FTS index", "error", err)
	}

	// Rebuild the ZIP cache since TIGER lookups depend on it.
	if err := s.geo.RebuildZipCache(ctx); err != nil {
		app.Logger().Warn("failed to rebuild zip cache", "error", err)
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

// Refresh downloads the latest OSM extract and re-indexes places.
//
// Data safety: This function does NOT clear the existing index before importing.
// Instead, it uses upserts (ON CONFLICT DO UPDATE), so if the import crashes
// mid-way, the database retains all previously-imported records plus any new
// records that were processed before the crash. The FTS index is rebuilt only
// after all records are successfully inserted, so a crash leaves the FTS in
// whatever state it was in before — which is still functional for the old data.
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

	parser := NewParser(s.geo, s.cfg.ImportBatchSize, s.cfg.OSMDecoderWorkers)
	if err := parser.Parse(ctx, f); err != nil {
		return fmt.Errorf("failed to parse osm data: %w", err)
	}

	return nil
}

func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
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
