package geocoder

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/sellography/geocoder-pb/internal/config"
	"github.com/sellography/geocoder-pb/internal/models"
)

// BBox represents a geographic bounding box for filtering search results.
// Coordinates use (lng, lat) ordering to match the Photon API convention.
type BBox struct {
	MinLng float64
	MinLat float64
	MaxLng float64
	MaxLat float64
}

// Valid returns true if the bounding box has been initialized with non-zero values.
func (b *BBox) Valid() bool {
	return b != nil && b.MinLng != 0 && b.MaxLng != 0
}

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
// If bbox is provided and valid, results are filtered to the bounding box.
func (g *Geocoder) Search(ctx context.Context, q string, limit int, bbox *BBox) ([]models.Place, error) {
	if limit <= 0 {
		limit = 10
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	// Use FTS5 to match the query across indexed fields.
	// When a bbox is provided, add a coordinate filter.
	bboxClause := ""
	params := dbx.Params{"query": q, "limit": limit}
	if bbox != nil && bbox.Valid() {
		bboxClause = `AND p.lat >= {:min_lat} AND p.lat <= {:max_lat}
			       AND p.lon >= {:min_lng} AND p.lon <= {:max_lng}`
		params["min_lat"] = bbox.MinLat
		params["max_lat"] = bbox.MaxLat
		params["min_lng"] = bbox.MinLng
		params["max_lng"] = bbox.MaxLng
	}

	query := fmt.Sprintf(`
		SELECT p.id, p.osm_id, p.osm_type, p.name, p.address, p.city, p.state,
		       p.postcode, p.country, p.lat, p.lon, p.class, p.type, p.importance,
		       p.created, p.updated
		FROM geocoder_places p
		INNER JOIN geocoder_places_fts f ON p.rowid = f.rowid
		WHERE geocoder_places_fts MATCH {:query}
		%s
		ORDER BY rank
		LIMIT {:limit}
	`, bboxClause)

	var places []models.Place
	if err := db.
		NewQuery(query).
		Bind(params).
		All(&places); err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	return places, nil
}

// Autocomplete performs a prefix-based search for autocomplete suggestions.
// It uses FTS5 prefix matching (e.g., "cal*" matches "Calico", "California").
// The query is tokenized and each token gets a * suffix for prefix matching.
// If bbox is provided and valid, results are filtered to the bounding box.
func (g *Geocoder) Autocomplete(ctx context.Context, q string, limit int, bbox *BBox) ([]models.Place, error) {
	if limit <= 0 {
		limit = 10
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	// Build FTS5 prefix query: "1600 pen*" -> "1600* pen*"
	ftsQuery := buildPrefixQuery(q)
	if ftsQuery == "" {
		return []models.Place{}, nil
	}

	// When a bbox is provided, add a coordinate filter.
	bboxClause := ""
	params := dbx.Params{"query": ftsQuery, "limit": limit}
	if bbox != nil && bbox.Valid() {
		bboxClause = `AND p.lat >= {:min_lat} AND p.lat <= {:max_lat}
			       AND p.lon >= {:min_lng} AND p.lon <= {:max_lng}`
		params["min_lat"] = bbox.MinLat
		params["max_lat"] = bbox.MaxLat
		params["min_lng"] = bbox.MinLng
		params["max_lng"] = bbox.MaxLng
	}

	query := fmt.Sprintf(`
		SELECT p.id, p.osm_id, p.osm_type, p.name, p.address, p.city, p.state,
		       p.postcode, p.country, p.lat, p.lon, p.class, p.type, p.importance,
		       p.created, p.updated
		FROM geocoder_places p
		INNER JOIN geocoder_places_fts f ON p.rowid = f.rowid
		WHERE geocoder_places_fts MATCH {:query}
		%s
		ORDER BY rank
		LIMIT {:limit}
	`, bboxClause)

	var places []models.Place
	if err := db.
		NewQuery(query).
		Bind(params).
		All(&places); err != nil {
		return nil, fmt.Errorf("autocomplete failed: %w", err)
	}

	return places, nil
}

// buildPrefixQuery converts a user query into an FTS5 prefix query.
// e.g., "1600 penn" -> "1600* penn*"
func buildPrefixQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}

	// Split on whitespace and add * suffix to each token.
	tokens := strings.Fields(q)
	for i, token := range tokens {
		// Don't add * if the token already ends with *.
		if !strings.HasSuffix(token, "*") {
			tokens[i] = token + "*"
		}
	}

	return strings.Join(tokens, " AND ")
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

// DropFTSTriggers drops the FTS5 sync triggers to speed up bulk inserts.
// Remember to call CreateFTSTriggers and RebuildFTS afterwards.
func (g *Geocoder) DropFTSTriggers(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	triggers := []string{"places_fts_insert", "places_fts_delete", "places_fts_update"}
	for _, t := range triggers {
		if _, err := db.NewQuery(fmt.Sprintf("DROP TRIGGER IF EXISTS %s", t)).Execute(); err != nil {
			return fmt.Errorf("failed to drop trigger %s: %w", t, err)
		}
	}

	return nil
}

// CreateFTSTriggers recreates the FTS5 sync triggers after bulk imports.
func (g *Geocoder) CreateFTSTriggers(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS places_fts_insert AFTER INSERT ON geocoder_places BEGIN
			INSERT INTO geocoder_places_fts(rowid, name, address, city, state, postcode)
			VALUES (new.rowid, new.name, new.address, new.city, new.state, new.postcode);
		END`,
		`CREATE TRIGGER IF NOT EXISTS places_fts_delete AFTER DELETE ON geocoder_places BEGIN
			INSERT INTO geocoder_places_fts(geocoder_places_fts, rowid, name, address, city, state, postcode)
			VALUES ('delete', old.rowid, old.name, old.address, old.city, old.state, old.postcode);
		END`,
		`CREATE TRIGGER IF NOT EXISTS places_fts_update AFTER UPDATE ON geocoder_places BEGIN
			INSERT INTO geocoder_places_fts(geocoder_places_fts, rowid, name, address, city, state, postcode)
			VALUES ('delete', old.rowid, old.name, old.address, old.city, old.state, old.postcode);
			INSERT INTO geocoder_places_fts(rowid, name, address, city, state, postcode)
			VALUES (new.rowid, new.name, new.address, new.city, new.state, new.postcode);
		END`,
	}

	for _, sql := range triggers {
		if _, err := db.NewQuery(sql).Execute(); err != nil {
			return fmt.Errorf("failed to create FTS trigger: %w", err)
		}
	}

	return nil
}

// NeedsFTSRebuild returns true if the places table has data but the FTS index is empty.
// This is a fast check using EXISTS instead of COUNT(*) to avoid scanning 54M rows.
func (g *Geocoder) NeedsFTSRebuild(ctx context.Context) (bool, error) {
	db := g.app.DB()
	if db == nil {
		return false, fmt.Errorf("db is not available")
	}

	// Fast check: does the places table have any rows?
	var hasPlaces int
	if err := db.NewQuery("SELECT EXISTS(SELECT 1 FROM geocoder_places LIMIT 1)").Row(&hasPlaces); err != nil {
		return false, fmt.Errorf("failed to check places: %w", err)
	}
	if hasPlaces == 0 {
		return false, nil // No places, no need to rebuild
	}

	// Fast check: does the FTS docsize table have any rows?
	// We check geocoder_places_fts_docsize (the internal shadow table that
	// stores per-document sizes) because
	// SELECT COUNT(*) FROM geocoder_places_fts can be slow on 54M rows.
	var hasFTS int
	if err := db.NewQuery("SELECT EXISTS(SELECT 1 FROM geocoder_places_fts_docsize LIMIT 1)").Row(&hasFTS); err != nil {
		// FTS docsize table might not exist; treat as needing rebuild.
		return true, nil
	}

	return hasFTS == 0, nil
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

// NodeCoord represents a node's OSM ID and coordinates for way centroid computation.
type NodeCoord struct {
	OSMID int64
	Lat   float64
	Lon   float64
}

// CreateNodeCoordTable creates a temporary table for storing node coordinates.
func (g *Geocoder) CreateNodeCoordTable(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	_, err := db.NewQuery(`CREATE TABLE IF NOT EXISTS _osm_node_coords (
osm_id INTEGER PRIMARY KEY,
lat REAL NOT NULL,
lon REAL NOT NULL
)`).Execute()
	if err != nil {
		return fmt.Errorf("failed to create node coord table: %w", err)
	}

	// Create index for faster lookups.
	_, _ = db.NewQuery("CREATE INDEX IF NOT EXISTS idx_node_coords_osm_id ON _osm_node_coords(osm_id)").Execute()

	return nil
}

// DropNodeCoordTable drops the temporary node coordinates table.
func (g *Geocoder) DropNodeCoordTable(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	_, err := db.NewQuery("DROP TABLE IF EXISTS _osm_node_coords").Execute()
	if err != nil {
		return fmt.Errorf("failed to drop node coord table: %w", err)
	}

	return nil
}

// BatchInsertNodeCoords inserts node coordinates in batches.
func (g *Geocoder) BatchInsertNodeCoords(ctx context.Context, coords []NodeCoord) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	txErr := g.app.RunInTransaction(func(txApp core.App) error {
		txDB := txApp.NonconcurrentDB()
		if txDB == nil {
			return fmt.Errorf("transaction db is not available")
		}

		query := `INSERT OR REPLACE INTO _osm_node_coords (osm_id, lat, lon) VALUES ({:osm_id}, {:lat}, {:lon})`

		for _, c := range coords {
			if _, err := txDB.NewQuery(query).Bind(dbx.Params{
				"osm_id": c.OSMID,
				"lat":    c.Lat,
				"lon":    c.Lon,
			}).Execute(); err != nil {
				return err
			}
		}

		return nil
	})

	if txErr != nil {
		return fmt.Errorf("batch insert node coords failed: %w", txErr)
	}

	return nil
}

// GetWayCentroid computes the centroid (average lat/lon) of a way from its node references.
func (g *Geocoder) GetWayCentroid(ctx context.Context, nodeIDs []int64) (float64, float64, error) {
	if len(nodeIDs) == 0 {
		return 0, 0, fmt.Errorf("no node IDs provided")
	}

	db := g.app.DB()
	if db == nil {
		return 0, 0, fmt.Errorf("db is not available")
	}

	// Build a comma-separated list of node IDs for the IN clause.
	// Use parameterized query to avoid SQL injection.
	placeholders := make([]string, len(nodeIDs))
	params := make(dbx.Params, len(nodeIDs))
	for i, id := range nodeIDs {
		key := fmt.Sprintf("n%d", i)
		placeholders[i] = fmt.Sprintf("{:%s}", key)
		params[key] = id
	}

	query := fmt.Sprintf(
		"SELECT AVG(lat) as avg_lat, AVG(lon) as avg_lon FROM _osm_node_coords WHERE osm_id IN (%s)",
		strings.Join(placeholders, ", "),
	)

	var result struct {
		AvgLat float64 `db:"avg_lat"`
		AvgLon float64 `db:"avg_lon"`
	}

	if err := db.NewQuery(query).Bind(params).One(&result); err != nil {
		return 0, 0, fmt.Errorf("failed to get way centroid: %w", err)
	}

	return result.AvgLat, result.AvgLon, nil
}
