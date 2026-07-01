package geocoder

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/sellography/geocoder-pb/internal/config"
	"github.com/sellography/geocoder-pb/internal/models"
)

// Geocoder provides address-to-coordinate lookup backed by SQLite/FTS5.
type Geocoder struct {
	app core.App
	cfg *config.Config
}

// New creates a new Geocoder instance.
func New(app core.App, cfg *config.Config) *Geocoder {
	return &Geocoder{app: app, cfg: cfg}
}

// Search performs a geocoding search for the given query.
func (g *Geocoder) Search(ctx context.Context, q string, limit int) ([]models.Place, error) {
	if limit <= 0 {
		limit = 10
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	// Use FTS5 to match the query across indexed fields.
	query := `
		SELECT p.id, p.osm_id, p.osm_type, p.name, p.address, p.city, p.state,
		       p.postcode, p.country, p.lat, p.lon, p.class, p.type, p.importance,
		       p.created, p.updated
		FROM geocoder_places p
		INNER JOIN geocoder_places_fts f ON p.rowid = f.rowid
		WHERE geocoder_places_fts MATCH {:query}
		ORDER BY rank
		LIMIT {:limit}
	`

	var places []models.Place
	if err := db.
		NewQuery(query).
		Bind(dbx.Params{"query": q, "limit": limit}).
		All(&places); err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	return places, nil
}

// Reverse performs a reverse geocoding lookup for the given coordinates.
func (g *Geocoder) Reverse(ctx context.Context, lat, lon float64, limit int) ([]models.Place, error) {
	if limit <= 0 {
		limit = 1
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	// Approximate nearest-neighbor using Haversine-like distance via simple lat/lon delta.
	query := `
		SELECT id, osm_id, osm_type, name, address, city, state, postcode, country,
		       lat, lon, class, type, importance, created, updated,
		       ((lat - {:lat}) * (lat - {:lat}) + (lon - {:lon}) * (lon - {:lon})) AS dist
		FROM geocoder_places
		ORDER BY dist
		LIMIT {:limit}
	`

	var places []models.Place
	if err := db.
		NewQuery(query).
		Bind(dbx.Params{"lat": lat, "lon": lon, "limit": limit}).
		All(&places); err != nil {
		return nil, fmt.Errorf("reverse geocoding failed: %w", err)
	}

	return places, nil
}

// UpsertPlace inserts or updates a single place in the geocoder index.
func (g *Geocoder) UpsertPlace(ctx context.Context, place *models.Place) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	query := `
		INSERT INTO geocoder_places (
			id, osm_id, osm_type, name, address, city, state, postcode, country,
			lat, lon, class, type, importance, created, updated
		) VALUES (
			{:id}, {:osm_id}, {:osm_type}, {:name}, {:address}, {:city}, {:state}, {:postcode}, {:country},
			{:lat}, {:lon}, {:class}, {:type}, {:importance}, datetime('now'), datetime('now')
		)
		ON CONFLICT(id) DO UPDATE SET
			osm_id = excluded.osm_id,
			osm_type = excluded.osm_type,
			name = excluded.name,
			address = excluded.address,
			city = excluded.city,
			state = excluded.state,
			postcode = excluded.postcode,
			country = excluded.country,
			lat = excluded.lat,
			lon = excluded.lon,
			class = excluded.class,
			type = excluded.type,
			importance = excluded.importance,
			updated = datetime('now')
	`

	_, err := db.NewQuery(query).Bind(dbx.Params{
		"id":         place.ID,
		"osm_id":     place.OSMID,
		"osm_type":   place.OSMType,
		"name":       place.Name,
		"address":    place.Address,
		"city":       place.City,
		"state":      place.State,
		"postcode":   place.Postcode,
		"country":    place.Country,
		"lat":        place.Lat,
		"lon":        place.Lon,
		"class":      place.Class,
		"type":       place.Type,
		"importance": place.Importance,
	}).Execute()

	if err != nil {
		return fmt.Errorf("upsert place failed: %w", err)
	}

	return nil
}

// BatchUpsertPlaces inserts or updates multiple places in batches within transactions.
// This is dramatically faster than individual UpsertPlace calls for bulk imports.
// Returns the number of places saved.
func (g *Geocoder) BatchUpsertPlaces(ctx context.Context, places []*models.Place, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 5000
	}

	saved := 0
	for i := 0; i < len(places); i += batchSize {
		if ctx.Err() != nil {
			return saved, ctx.Err()
		}

		end := i + batchSize
		if end > len(places) {
			end = len(places)
		}
		batch := places[i:end]

		txErr := g.app.RunInTransaction(func(txApp core.App) error {
			txDB := txApp.NonconcurrentDB()
			if txDB == nil {
				return fmt.Errorf("transaction db is not available")
			}

			query := `
				INSERT INTO geocoder_places (
					id, osm_id, osm_type, name, address, city, state, postcode, country,
					lat, lon, class, type, importance, created, updated
				) VALUES (
					{:id}, {:osm_id}, {:osm_type}, {:name}, {:address}, {:city}, {:state}, {:postcode}, {:country},
					{:lat}, {:lon}, {:class}, {:type}, {:importance}, datetime('now'), datetime('now')
				)
				ON CONFLICT(id) DO UPDATE SET
					osm_id = excluded.osm_id,
					osm_type = excluded.osm_type,
					name = excluded.name,
					address = excluded.address,
					city = excluded.city,
					state = excluded.state,
					postcode = excluded.postcode,
					country = excluded.country,
					lat = excluded.lat,
					lon = excluded.lon,
					class = excluded.class,
					type = excluded.type,
					importance = excluded.importance,
					updated = datetime('now')
			`

			for _, place := range batch {
				if _, err := txDB.NewQuery(query).Bind(dbx.Params{
					"id":         place.ID,
					"osm_id":     place.OSMID,
					"osm_type":   place.OSMType,
					"name":       place.Name,
					"address":    place.Address,
					"city":       place.City,
					"state":      place.State,
					"postcode":   place.Postcode,
					"country":    place.Country,
					"lat":        place.Lat,
					"lon":        place.Lon,
					"class":      place.Class,
					"type":       place.Type,
					"importance": place.Importance,
				}).Execute(); err != nil {
					return err
				}
			}

			return nil
		})

		if txErr != nil {
			return saved, fmt.Errorf("batch upsert failed at offset %d: %w", i, txErr)
		}

		saved += len(batch)
	}

	return saved, nil
}

// ClearIndex removes all indexed places.
func (g *Geocoder) ClearIndex(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	// Delete all records. The FTS triggers keep the FTS table in sync.
	if _, err := db.NewQuery("DELETE FROM geocoder_places").Execute(); err != nil {
		return fmt.Errorf("clear index failed: %w", err)
	}

	return nil
}

// Count returns the number of indexed places.
func (g *Geocoder) Count(ctx context.Context) (int64, error) {
	db := g.app.DB()
	if db == nil {
		return 0, fmt.Errorf("db is not available")
	}

	var count int64
	if err := db.NewQuery("SELECT COUNT(*) FROM geocoder_places").Row(&count); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("count failed: %w", err)
	}

	return count, nil
}

// RebuildFTS rebuilds the FTS5 index from the existing geocoder_places data.
// This is useful after bulk imports that bypassed the FTS triggers.
func (g *Geocoder) RebuildFTS(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	if _, err := db.NewQuery("INSERT INTO geocoder_places_fts(geocoder_places_fts) VALUES('rebuild')").Execute(); err != nil {
		return fmt.Errorf("rebuild FTS failed: %w", err)
	}

	return nil
}

// LogProgress logs import progress at regular intervals.
func (g *Geocoder) LogProgress(count int, startTime time.Time) {
	elapsed := time.Since(startTime)
	g.app.Logger().Info("osm import progress",
		"indexed", count,
		"elapsed", elapsed.Round(time.Second).String(),
		"rate", fmt.Sprintf("%.0f/s", float64(count)/elapsed.Seconds()),
	)
}

// AppLogger returns the PocketBase app logger for use by external components.
func (g *Geocoder) AppLogger() *slog.Logger {
	return g.app.Logger()
}
