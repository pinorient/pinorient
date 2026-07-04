package main

import (
	"context"
	"log"

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

func main() {
	// Load .env file if it exists. This allows configuration via a .env file
	// instead of requiring environment variables to be set explicitly.
	// Existing environment variables take precedence over .env values.
	_ = godotenv.Load()

	// Use NewWithConfig to provide a custom DBConnect function with optimized pragmas
	// for the 16GB geocoder database. The default PocketBase pragmas use a 32MB cache
	// and no mmap, which is too small for 54M rows.
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DBConnect: func(dbPath string) (*dbx.DB, error) {
			pragmas := "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(200000000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-262144)&_pragma=mmap_size(4294967296)"
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
