package pinorient

import (
	"context"
	"embed"
	"io/fs"
	"net/http"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/pinorient/pinorient/internal/api"
	"github.com/pinorient/pinorient/internal/config"
	"github.com/pinorient/pinorient/internal/db"
	"github.com/pinorient/pinorient/internal/geocoder"
	"github.com/pinorient/pinorient/internal/osm"
)

//go:embed homepage
var defaultHomepage embed.FS

// Setup wires the pinorient geocoder into the given PocketBase app: it runs
// database migrations, registers the /api/geocoder routes, serves the static
// homepage at / (unless disabled), and schedules OSM/TIGER indexing and
// refresh. Downstream applications call Setup, optionally register their own
// routes afterwards, and then call app.Start().
func Setup(app *pocketbase.PocketBase, opts ...Option) error {
	o := &options{homepageEnable: true}
	for _, opt := range opts {
		opt(o)
	}

	// Bootstrap the app so the DB and settings are available.
	if !app.IsBootstrapped() {
		if err := app.Bootstrap(); err != nil {
			return err
		}
	}

	cfg := o.cfg
	if cfg == nil {
		cfg = config.Load()
	}

	checker := o.domainChecker
	if checker == nil {
		checker = api.EnvDomainChecker(cfg.AllowedDomains)
	}

	homepage := o.homepage
	if homepage == nil && o.homepageEnable {
		sub, err := fs.Sub(defaultHomepage, "homepage")
		if err != nil {
			return err
		}
		homepage = sub
	}

	// Initialize the geocoder service.
	geo := geocoder.New(app, cfg)

	// Run database migrations before starting the server.
	if err := db.RunMigrations(app); err != nil {
		return err
	}

	scheduler := osm.NewScheduler(cfg, geo)

	// Register API routes and periodic OSM refresh when the web server starts.
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		api.RegisterRoutes(e, geo, checker, o.middleware...)

		if homepage != nil {
			registerHomepage(e, homepage)
		}

		startIndexing(e, app, cfg, scheduler, geo)

		if cfg.UpdateCron != "" {
			if err := scheduler.Start(context.Background(), app); err != nil {
				return err
			}
		}

		return e.Next()
	})

	return nil
}

// registerHomepage serves index.html from the given filesystem at / and any
// additional assets under /static/.
func registerHomepage(e *core.ServeEvent, homepage fs.FS) {
	e.Router.GET("/", func(c *core.RequestEvent) error {
		data, err := fs.ReadFile(homepage, "index.html")
		if err != nil {
			return c.NotFoundError("homepage not found", err)
		}
		return c.HTML(http.StatusOK, string(data))
	})
	e.Router.GET("/static/{path...}", func(c *core.RequestEvent) error {
		http.StripPrefix("/static/", http.FileServer(http.FS(homepage))).ServeHTTP(c.Response, c.Request)
		return nil
	})
}

// startIndexing kicks off the background OSM and TIGER imports, either
// sequentially (low-memory default) or concurrently based on config.
func startIndexing(e *core.ServeEvent, app *pocketbase.PocketBase, cfg *config.Config, scheduler *osm.Scheduler, geo *geocoder.Geocoder) {
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
		return
	}

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
