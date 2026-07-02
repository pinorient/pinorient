package osm

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/pocketbase/pocketbase/core"

	"github.com/sellography/geocoder-pb/internal/config"
	"github.com/sellography/geocoder-pb/internal/geocoder"
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
func (s *Scheduler) EnsureIndexed(ctx context.Context, app core.App) error {
	count, err := s.geo.Count(ctx)
	if err != nil {
		return fmt.Errorf("failed to check index count: %w", err)
	}

	if count > 0 && !s.cfg.ForceReindex {
		app.Logger().Info("geocoder index already populated", "count", count)
		return nil
	}

	if s.cfg.ForceReindex {
		app.Logger().Info("force reindex enabled; re-importing OSM data...", "current_count", count)
	} else {
		app.Logger().Info("geocoder index empty; fetching OSM data...")
	}

	return s.Refresh(ctx)
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
