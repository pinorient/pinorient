package main

import (
	"context"
	"log"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/sellography/geocoder-pb/internal/api"
	"github.com/sellography/geocoder-pb/internal/config"
	"github.com/sellography/geocoder-pb/internal/db"
	"github.com/sellography/geocoder-pb/internal/geocoder"
	"github.com/sellography/geocoder-pb/internal/osm"
)

func main() {
	cfg := config.Load()

	app := pocketbase.New()

	// Initialize the geocoder service.
	geo := geocoder.New(app, cfg)

	// Bootstrap the app so the DB and settings are available.
	if err := app.Bootstrap(); err != nil {
		log.Fatal(err)
	}

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
