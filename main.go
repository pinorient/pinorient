package main

import (
	"context"
	"fmt"
	"log"
	"strconv"

	"os"

	"github.com/joho/godotenv"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/sellography/geocoder-pb/internal/api"
	"github.com/sellography/geocoder-pb/internal/config"
	"github.com/sellography/geocoder-pb/internal/db"
	"github.com/sellography/geocoder-pb/internal/geocoder"
	"github.com/sellography/geocoder-pb/internal/osm"
)

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func main() {
	// Load .env file if it exists. This allows configuration via a .env file
	// instead of requiring environment variables to be set explicitly.
	// Existing environment variables take precedence over .env values.
	_ = godotenv.Load()

	// Use NewWithConfig to provide a custom DBConnect function with optimized pragmas
	// for the geocoder database. Cache size and mmap size are configurable via
	// environment variables to support servers with different amounts of RAM.
	// Defaults: cache_size=64MB, mmap_size=0 (disabled) — safe for 2GB RAM servers.
	// For machines with 8GB+ RAM, set DB_CACHE_SIZE=262144 and DB_MMAP_SIZE=4294967296.
	cacheSize := getEnvInt("DB_CACHE_SIZE", 65536) // in KB, 65536 = 64MB
	mmapSize := getEnvInt64("DB_MMAP_SIZE", 0)     // in bytes, 0 = disabled
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DBConnect: func(dbPath string) (*dbx.DB, error) {
			pragmas := fmt.Sprintf("?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(200000000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-%d)&_pragma=mmap_size(%d)", cacheSize, mmapSize)
			return dbx.Open("sqlite", dbPath+pragmas)
		},
	})

	// Bootstrap the app so the DB and settings are available.
	// Config is loaded after godotenv.Load() above, which populates environment
	// variables from the .env file. Existing env vars take precedence.
	if err := app.Bootstrap(); err != nil {
		log.Fatal(err)
	}

	cfg := config.Load()

	// Initialize the geocoder service.
	geo := geocoder.New(app, cfg)

	// Run database migrations before starting the server.
	if err := db.RunMigrations(app); err != nil {
		log.Fatal(err)
	}

	scheduler := osm.NewScheduler(cfg, geo)

	// Register API routes and periodic OSM refresh when the web server starts.
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		api.RegisterRoutes(e, geo, cfg)

		// Run imports either sequentially or concurrently based on config.
		// On low-memory servers (2GB RAM), sequential mode halves peak memory
		// usage by ensuring OSM and TIGER imports don't run simultaneously.
		if cfg.SerializeImports {
			go func() {
				// OSM first, then TIGER.
				if err := scheduler.EnsureIndexed(context.Background(), app); err != nil {
					app.Logger().Error("osm indexing failed", "error", err)
				}
				if err := scheduler.EnsureTigerIndexed(context.Background(), app); err != nil {
					app.Logger().Error("tiger indexing failed", "error", err)
				}
			}()
		} else {
			// Index OSM data in the background so the server starts immediately.
			go func() {
				if err := scheduler.EnsureIndexed(context.Background(), app); err != nil {
					app.Logger().Error("osm indexing failed", "error", err)
				}
			}()

			// Index TIGER/Line address data in the background for address interpolation.
			go func() {
				if err := scheduler.EnsureTigerIndexed(context.Background(), app); err != nil {
					app.Logger().Error("tiger indexing failed", "error", err)
				}
			}()
		}

		if cfg.UpdateCron != "" {
			if err := scheduler.Start(context.Background(), app); err != nil {
				return err
			}
		}

		return e.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
