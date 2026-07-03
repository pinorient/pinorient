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
		return nil
	}

	// FTS has data — check if we need a full re-import.
	if s.cfg.ForceReindex {
		app.Logger().Info("force reindex enabled; re-importing OSM data...")
		return s.Refresh(ctx)
	}

	// Everything is already populated.
	app.Logger().Info("geocoder index already populated")
	return nil
}

// EnsureTigerIndexed downloads and imports TIGER/Line address range data
// for the configured counties if the tiger_addr_ranges table is empty.
func (s *Scheduler) EnsureTigerIndexed(ctx context.Context, app core.App) error {
	if len(s.cfg.TIGERCounties) == 0 {
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

	app.Logger().Info("importing TIGER/Line address data", "counties", s.cfg.TIGERCounties)
	if err := s.ImportTiger(ctx, app); err != nil {
		return fmt.Errorf("TIGER import failed: %w", err)
	}
	app.Logger().Info("TIGER/Line import complete")
	return nil
}

// ImportTiger downloads and imports TIGER/Line ADDRFEAT shapefiles for all configured counties.
func (s *Scheduler) ImportTiger(ctx context.Context, app core.App) error {
	if err := os.MkdirAll(s.cfg.OSMDataPath, 0o755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	tigerDir := filepath.Join(s.cfg.OSMDataPath, "tiger")
	if err := os.MkdirAll(tigerDir, 0o755); err != nil {
		return fmt.Errorf("failed to create tiger data directory: %w", err)
	}

	for _, fips := range s.cfg.TIGERCounties {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		zipPath := filepath.Join(tigerDir, fmt.Sprintf("tl_%s_%s_addrfeat.zip", s.cfg.TIGERYear, fips))
		extractDir := filepath.Join(tigerDir, fips)

		// Download the ZIP if it doesn't exist.
		if _, err := os.Stat(zipPath); os.IsNotExist(err) {
			url := fmt.Sprintf("https://www2.census.gov/geo/tiger/TIGER%s/ADDRFEAT/tl_%s_%s_addrfeat.zip",
				s.cfg.TIGERYear, s.cfg.TIGERYear, fips)
			log.Printf("downloading TIGER/Line data from %s...", url)
			if err := downloadFile(ctx, url, zipPath); err != nil {
				return fmt.Errorf("failed to download TIGER data for county %s: %w", fips, err)
			}
			log.Printf("TIGER/Line data downloaded for county %s", fips)
		} else {
			log.Printf("using existing TIGER/Line data for county %s", fips)
		}

		// Extract the ZIP if the directory doesn't exist.
		if _, err := os.Stat(extractDir); os.IsNotExist(err) {
			if err := unzipFile(zipPath, extractDir); err != nil {
				return fmt.Errorf("failed to unzip TIGER data for county %s: %w", fips, err)
			}
		}

		// Parse and import the shapefile.
		parser := tiger.NewParser(s.geo)
		if err := parser.ParseDir(ctx, extractDir); err != nil {
			return fmt.Errorf("failed to parse TIGER data for county %s: %w", fips, err)
		}
	}

	// Rebuild the TIGER FTS index after import.
	if err := s.geo.RebuildTigerFTS(ctx); err != nil {
		app.Logger().Warn("failed to rebuild TIGER FTS index", "error", err)
	}

	return nil
}

// Refresh downloads the latest OSM extract and re-indexes places.
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

	if err := s.geo.ClearIndex(ctx); err != nil {
		return fmt.Errorf("failed to clear index: %w", err)
	}

	parser := NewParser(s.geo)
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
