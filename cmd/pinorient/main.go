package main

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strconv"

	"os"

	"github.com/joho/godotenv"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"

	"github.com/pocketbase/pocketbase/core"

	"github.com/pinorient/pinorient/internal/api"
	appconfig "github.com/pinorient/pinorient/internal/config"
	"github.com/pinorient/pinorient/internal/db"
	"github.com/pinorient/pinorient/internal/geocoder"
	"github.com/pinorient/pinorient/internal/osm"
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

	// Cap the Go heap so the garbage collector works harder before the kernel
	// OOM killer gets involved. This matters on small (2GB RAM) servers where
	// the default GC behavior lets the heap grow large enough to get the
	// process killed mid-import. The SQLite driver (modernc.org/sqlite) is
	// pure Go, so its page cache is covered by this limit too.
	//
	// The standard GOMEMLIMIT environment variable (e.g. "1800MiB") always
	// wins when set; otherwise GO_MEM_LIMIT_MB applies, defaulting to a
	// 2GB-server-safe 1500MiB. Set GOMEMLIMIT explicitly on larger servers.
	if os.Getenv("GOMEMLIMIT") == "" {
		limitMB := getEnvInt64("GO_MEM_LIMIT_MB", 1500)
		debug.SetMemoryLimit(limitMB << 20)
		log.Printf("go memory limit set to %dMiB (override with GOMEMLIMIT or GO_MEM_LIMIT_MB)", limitMB)
	}

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

	cfg := appconfig.Load()
	geo := geocoder.New(app, cfg)

	if err := db.RunMigrations(app); err != nil {
		log.Fatal(err)
	}

	scheduler := osm.NewScheduler(cfg, geo)

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
				// Remove the pre-migration TIGER table (if any) in bounded
				// chunks before the TIGER phase starts writing.
				if err := geo.CleanupOldTigerTable(context.Background()); err != nil {
					app.Logger().Error("tiger pre-migration cleanup failed", "error", err)
				}
				if err := scheduler.EnsureTigerIndexed(context.Background(), app); err != nil {
					app.Logger().Error("tiger indexing failed", "error", err)
				}
			}()
		} else {
			go func() {
				if err := scheduler.EnsureIndexed(context.Background(), app); err != nil {
					app.Logger().Error("osm indexing failed", "error", err)
				}
			}()

			// Remove the pre-migration TIGER table (if any) in bounded chunks.
			go func() {
				if err := geo.CleanupOldTigerTable(context.Background()); err != nil {
					app.Logger().Error("tiger pre-migration cleanup failed", "error", err)
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
