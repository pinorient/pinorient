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

	// Run database migrations, register API routes, and schedule OSM refresh
	// when the web server starts.
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		if err := db.RunMigrations(app); err != nil {
			return err
		}

		api.RegisterRoutes(e, geo, cfg)

		if cfg.UpdateCron != "" {
			scheduler := osm.NewScheduler(cfg, geo)
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
