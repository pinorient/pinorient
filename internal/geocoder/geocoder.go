package geocoder

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
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

// parseHouseNumber attempts to extract a leading house number from a query.
// Returns the house number, the remaining street name, and true if a house
// number was found. e.g., "42 Maple St" -> (42, "Maple St", true).
func parseHouseNumber(q string) (int, string, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, "", false
	}

	tokens := strings.Fields(q)
	if len(tokens) < 2 {
		return 0, "", false
	}

	n, err := strconv.Atoi(tokens[0])
	if err != nil || n <= 0 {
		return 0, "", false
	}

	streetName := strings.Join(tokens[1:], " ")
	if streetName == "" {
		return 0, "", false
	}

	return n, streetName, true
}

// Search performs a geocoding search for the given query.
// If the query starts with a house number, TIGER/Line address interpolation
// is attempted first to fill coverage gaps. Results from OSM FTS are also included.
// If bbox is provided and valid, results are filtered to the bounding box.
func (g *Geocoder) Search(ctx context.Context, q string, limit int, bbox *BBox) ([]models.Place, error) {
	if limit <= 0 {
		limit = 10
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	var places []models.Place

	// Try TIGER address interpolation if the query starts with a house number.
	if houseNum, streetName, ok := parseHouseNumber(q); ok {
		if tigerPlaces, err := g.InterpolateAddress(ctx, houseNum, streetName, limit); err == nil {
			for i := range tigerPlaces {
				tp := &tigerPlaces[i]
				// Apply bbox filter to TIGER result if needed.
				if bbox == nil || !bbox.Valid() ||
					(tp.Lat >= bbox.MinLat && tp.Lat <= bbox.MaxLat &&
						tp.Lon >= bbox.MinLng && tp.Lon <= bbox.MaxLng) {
					places = append(places, *tp)
				}
			}
		}
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
		ORDER BY bm25(geocoder_places_fts, 10.0, 1.0, 1.0, 1.0, 1.0)
	`, bboxClause)

	var osmPlaces []models.Place
	if err := db.
		NewQuery(query).
		Bind(params).
		All(&osmPlaces); err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	places = append(places, osmPlaces...)
	if len(places) > limit {
		places = places[:limit]
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
	`, bboxClause)

	var places []models.Place
	if err := db.
		NewQuery(query).
		Bind(params).
		All(&places); err != nil {
		return nil, fmt.Errorf("autocomplete failed: %w", err)
	}

	// Try TIGER address interpolation if the query starts with a house number.
	// This fills coverage gaps for addresses that exist in TIGER but not OSM.
	// Limit TIGER results so OSM results still appear in autocomplete.
	if houseNum, streetName, ok := parseHouseNumber(q); ok {
		// Limit TIGER results to at most half the limit so OSM results
		// still appear in autocomplete.
		maxTiger := limit / 2
		if maxTiger < 3 {
			maxTiger = 3
		}
		if tigerPlaces, err := g.InterpolateAddress(ctx, houseNum, streetName, maxTiger); err == nil {
			var filtered []models.Place
			for i := range tigerPlaces {
				tp := &tigerPlaces[i]
				// Apply bbox filter to TIGER result if needed.
				if bbox == nil || !bbox.Valid() ||
					(tp.Lat >= bbox.MinLat && tp.Lat <= bbox.MaxLat &&
						tp.Lon >= bbox.MinLng && tp.Lon <= bbox.MaxLng) {
					filtered = append(filtered, *tp)
				}
			}
			places = append(filtered, places...)
			if len(places) > limit {
				places = places[:limit]
			}
		}
	}

	return places, nil
}

// buildPrefixQuery converts a user query into an FTS5 prefix query.
// Only the last token gets a prefix wildcard (for autocomplete), while earlier
// tokens require exact matches. Tokens shorter than 3 characters are skipped
// entirely to avoid matching millions of rows (e.g., "st*" matches 12M rows).
// The last token only gets a wildcard if it is 3-5 chars (likely still being typed).
// Tokens of 6+ chars are treated as complete words (exact match) to avoid
// matching millions of rows for common words like "street*" (10M+ matches).
// e.g., "12 Warren St" -> "warren"
// e.g., "12 Warren Stre" -> "warren" AND "stre*"
// e.g., "12 Warren Street" -> "warren" AND "street"
func buildPrefixQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}

	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return ""
	}

	var parts []string
	for i, token := range tokens {
		isLast := i == len(tokens)-1
		// Skip very short tokens entirely.
		if len(token) < 3 {
			continue
		}
		if isLast && len(token) <= 5 && !strings.HasSuffix(token, "*") {
			// Last token, 3-5 chars: prefix match for autocomplete
			parts = append(parts, token+"*")
		} else if isLast && len(token) <= 5 && strings.HasSuffix(token, "*") {
			parts = append(parts, token)
		} else {
			// Earlier tokens or long last token: exact match
			parts = append(parts, "\""+token+"\"")
		}
	}

	return strings.Join(parts, " AND ")
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

// HasPlaces returns true if the geocoder_places table has any rows.
// Uses a fast EXISTS check instead of COUNT(*) to avoid scanning 54M rows.
func (g *Geocoder) HasPlaces(ctx context.Context) (bool, error) {
	db := g.app.DB()
	if db == nil {
		return false, fmt.Errorf("db is not available")
	}

	var hasRows int
	if err := db.NewQuery("SELECT EXISTS(SELECT 1 FROM geocoder_places LIMIT 1)").Row(&hasRows); err != nil {
		return false, fmt.Errorf("failed to check places: %w", err)
	}
	return hasRows == 1, nil
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

// HasZipCache returns true if the zip_city_state cache table has any rows.
func (g *Geocoder) HasZipCache(ctx context.Context) (bool, error) {
	db := g.app.DB()
	if db == nil {
		return false, fmt.Errorf("db is not available")
	}

	var hasRows int
	if err := db.NewQuery("SELECT EXISTS(SELECT 1 FROM zip_city_state LIMIT 1)").Row(&hasRows); err != nil {
		return false, fmt.Errorf("failed to check zip cache: %w", err)
	}
	return hasRows == 1, nil
}

// RebuildZipCache rebuilds the zip_city_state cache table from geocoder_places.
// This table maps ZIP codes to their most common city/state, enabling fast
// JOINs in TIGER address interpolation instead of N+1 per-row lookups.
func (g *Geocoder) RebuildZipCache(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	// Drop and recreate the cache table.
	if _, err := db.NewQuery("DROP TABLE IF EXISTS zip_city_state").Execute(); err != nil {
		return fmt.Errorf("failed to drop zip cache: %w", err)
	}

	if _, err := db.NewQuery(`
		CREATE TABLE zip_city_state AS
		SELECT postcode, city, state FROM (
			SELECT postcode, city, state, COUNT(*) AS cnt,
			       ROW_NUMBER() OVER (PARTITION BY postcode ORDER BY COUNT(*) DESC) as rn
			FROM geocoder_places
			WHERE city != '' AND postcode != ''
			GROUP BY postcode, city, state
		) WHERE rn = 1
	`).Execute(); err != nil {
		return fmt.Errorf("failed to build zip cache: %w", err)
	}

	if _, err := db.NewQuery("CREATE INDEX IF NOT EXISTS idx_zip_city_state ON zip_city_state(postcode)").Execute(); err != nil {
		return fmt.Errorf("failed to create zip cache index: %w", err)
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

// AddrRange represents a TIGER/Line address range record.
type AddrRange struct {
	FullName string  // Street name (e.g., "Maple St")
	FromHN   int     // From house number
	ToHN     int     // To house number
	Parity   string  // "E" (even) or "O" (odd)
	ZIP      string  // ZIP code
	Side     string  // "L" (left) or "R" (right)
	Lat      float64 // Midpoint latitude
	Lon      float64 // Midpoint longitude
}

// HasTigerAddrRanges returns true if the tiger_addr_ranges table has any data.
func (g *Geocoder) HasTigerAddrRanges(ctx context.Context) (bool, error) {
	db := g.app.DB()
	if db == nil {
		return false, fmt.Errorf("db is not available")
	}

	var hasRows int
	if err := db.NewQuery("SELECT EXISTS(SELECT 1 FROM tiger_addr_ranges LIMIT 1)").Row(&hasRows); err != nil {
		return false, fmt.Errorf("failed to check tiger addr ranges: %w", err)
	}
	return hasRows == 1, nil
}

// RebuildTigerFTS rebuilds the TIGER FTS5 index from the tiger_addr_ranges table.
func (g *Geocoder) RebuildTigerFTS(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	if _, err := db.NewQuery("INSERT INTO tiger_addr_fts(tiger_addr_fts) VALUES('rebuild')").Execute(); err != nil {
		return fmt.Errorf("rebuild TIGER FTS failed: %w", err)
	}
	return nil
}

// normalizeStreetName expands common abbreviations so that user queries like
// "Maple Street" match TIGER data which uses "Maple St".
func normalizeStreetName(name string) string {
	name = strings.TrimSpace(name)
	// Replace whole-word abbreviations (case-insensitive).
	replacements := []struct{ full, abbr string }{
		{"Street", "St"},
		{"Avenue", "Ave"},
		{"Boulevard", "Blvd"},
		{"Drive", "Dr"},
		{"Road", "Rd"},
		{"Lane", "Ln"},
		{"Court", "Ct"},
		{"Place", "Pl"},
		{"Circle", "Cir"},
	}
	words := strings.Fields(name)
	for i, w := range words {
		for _, r := range replacements {
			if strings.EqualFold(w, r.full) {
				words[i] = r.abbr
			}
		}
	}
	return strings.Join(words, " ")
}

// titleCase capitalizes the first letter of each word, leaving the rest lowercase.
// This matches TIGER/Line's naming convention (e.g., "Birchwood Rd", "Main St")
// so that case-sensitive index lookups work with lowercase user input.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
		}
	}
	return strings.Join(words, " ")
}

// tigerAddrRow represents a row from the tiger_addr_ranges table.
type tigerAddrRow struct {
	FullName string  `db:"full_name"`
	FromHN   int     `db:"from_hn"`
	ToHN     int     `db:"to_hn"`
	Parity   string  `db:"parity"`
	ZIP      string  `db:"zip"`
	Side     string  `db:"side"`
	Lat      float64 `db:"lat"`
	Lon      float64 `db:"lon"`
	City     string  `db:"city"`
	State    string  `db:"state"`
}

// InterpolateAddress looks up a house number on a named street in TIGER data
// and returns all matching interpolated coordinates. Returns an empty slice if
// no match is found. Handles abbreviation differences (e.g., "Maple Street"
// matches "Maple St").
//
// A single street name + house number can match multiple address ranges across
// different cities/states (e.g., "11 Englewood Ave" exists in Brookline MA,
// Bloomfield CT, and many other towns). All matches are returned so the caller
// can present them as autocomplete options.
//
// The limit parameter controls the maximum number of results returned, which
// prevents fetching and enriching thousands of rows for common street names.
func (g *Geocoder) InterpolateAddress(ctx context.Context, houseNumber int, streetName string, limit int) ([]models.Place, error) {
	if limit <= 0 {
		limit = 10
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	// Determine parity (odd/even) of the house number.
	parity := "O"
	if houseNumber%2 == 0 {
		parity = "E"
	}

	// Derive state from the configured county FIPS codes.
	// The first 2 digits of a county FIPS code are the state FIPS.
	// When TIGER_ALL_COUNTIES is set, TIGERCounties is empty, so we fall
	// back to the state from the zip_city_state cache (via the JOIN).
	defaultState := fipsToState(g.cfg.TIGERCounties)

	// Track which TIGER rows we've already emitted (dedup by full_name+zip+side).
	seen := make(map[string]bool)
	var results []models.Place

	// Helper to build Place structs from tigerAddrRow results.
	buildPlaces := func(rows []tigerAddrRow) {
		for _, ar := range rows {
			dedupKey := ar.FullName + "|" + ar.ZIP + "|" + ar.Side
			if seen[dedupKey] {
				continue
			}
			seen[dedupKey] = true

			rowState := ar.State
			if rowState == "" {
				rowState = defaultState
			}

			place := models.Place{
				ID:       fmt.Sprintf("tiger-%d-%s-%s-%s", houseNumber, ar.FullName, ar.ZIP, ar.Side),
				Name:     fmt.Sprintf("%d %s", houseNumber, streetName),
				Address:  fmt.Sprintf("%d %s", houseNumber, streetName),
				City:     ar.City,
				State:    rowState,
				Postcode: ar.ZIP,
				Country:  "US",
				Lat:      ar.Lat,
				Lon:      ar.Lon,
				Class:    "highway",
				Type:     "residential",
			}
			results = append(results, place)
		}
	}

	// 1. Try exact match on original and normalized street names.
	// This handles cases where the user types "Maple Street" but TIGER has "Maple St".
	// No LIMIT here — the full_name index makes this fast, and we need all matches so the
	// caller can present different cities/states as autocomplete options.
	// Title-case the street name to match TIGER's naming convention (e.g., "Birchwood Rd").
	// This enables case-sensitive index lookups which are much faster than COLLATE NOCASE.
	titleCased := titleCase(streetName)
	normalized := titleCase(normalizeStreetName(streetName))
	exactQuery := `
		SELECT t.full_name, t.from_hn, t.to_hn, t.parity, t.zip, t.side, t.lat, t.lon,
		       z.city, z.state
		FROM tiger_addr_ranges t
		LEFT JOIN zip_city_state z ON z.postcode = t.zip
		WHERE t.full_name IN ({:street1}, {:street2})
		  AND ((t.from_hn <= {:hn} AND t.to_hn >= {:hn}) OR (t.to_hn <= {:hn} AND t.from_hn >= {:hn}))
		  AND (t.parity = {:parity} OR t.parity = 'B')
		ORDER BY t.rowid
	`
	var exactRows []tigerAddrRow
	if err := db.NewQuery(exactQuery).Bind(dbx.Params{
		"street1": titleCased,
		"street2": normalized,
		"hn":      houseNumber,
		"parity":  parity,
	}).All(&exactRows); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("tiger address lookup failed: %w", err)
	}
	buildPlaces(exactRows)

	// If we have enough results, return early.
	if len(results) >= limit {
		return results[:limit], nil
	}

	// 2. Fall back to FTS prefix match on the first word.
	// This uses the tiger_addr_fts index instead of a LIKE full table scan.
	// e.g., "main*" matches "Main St", "Main Rd", "Maine St", etc.
	if parts := strings.Fields(streetName); len(parts) > 0 {
		remaining := limit - len(results)
		ftsQuery := parts[0] + "*"
		ftsQuerySQL := `
			SELECT t.full_name, t.from_hn, t.to_hn, t.parity, t.zip, t.side, t.lat, t.lon,
			       z.city, z.state
			FROM tiger_addr_ranges t
			INNER JOIN tiger_addr_fts f ON t.rowid = f.rowid
			LEFT JOIN zip_city_state z ON z.postcode = t.zip
			WHERE tiger_addr_fts MATCH {:fts_query}
			  AND ((t.from_hn <= {:hn} AND t.to_hn >= {:hn}) OR (t.to_hn <= {:hn} AND t.from_hn >= {:hn}))
			  AND (t.parity = {:parity} OR t.parity = 'B')
		LIMIT {:limit}
		`
		var ftsRows []tigerAddrRow
		if err := db.NewQuery(ftsQuerySQL).Bind(dbx.Params{
			"fts_query": ftsQuery,
			"hn":        houseNumber,
			"parity":    parity,
			"limit":     remaining,
		}).All(&ftsRows); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("tiger address lookup failed: %w", err)
		}
		buildPlaces(ftsRows)
	}

	return results, nil
}

// fipsToState maps a county FIPS code's state portion (first 2 digits) to a
// 2-letter state abbreviation. Returns "" if the state is unknown.
func fipsToState(countyFIPS []string) string {
	if len(countyFIPS) == 0 {
		return ""
	}
	// State FIPS is the first 2 digits of the county FIPS code.
	if len(countyFIPS[0]) < 2 {
		return ""
	}
	stateFIPS := countyFIPS[0][:2]
	stateMap := map[string]string{
		"01": "AL", "02": "AK", "04": "AZ", "05": "AR", "06": "CA",
		"08": "CO", "09": "CT", "10": "DE", "11": "DC", "12": "FL",
		"13": "GA", "15": "HI", "16": "ID", "17": "IL", "18": "IN",
		"19": "IA", "20": "KS", "21": "KY", "22": "LA", "23": "ME",
		"24": "MD", "25": "MA", "26": "MI", "27": "MN", "28": "MS",
		"29": "MO", "30": "MT", "31": "NE", "32": "NV", "33": "NH",
		"34": "NJ", "35": "NM", "36": "NY", "37": "NC", "38": "ND",
		"39": "OH", "40": "OK", "41": "OR", "42": "PA", "44": "RI",
		"45": "SC", "46": "SD", "47": "TN", "48": "TX", "49": "UT",
		"50": "VT", "51": "VA", "53": "WA", "54": "WV", "55": "WI",
		"56": "WY",
	}
	return stateMap[stateFIPS]
}

// BatchUpsertAddrRanges inserts or updates TIGER/Line address ranges in batches.
func (g *Geocoder) BatchUpsertAddrRanges(ctx context.Context, ranges []*AddrRange, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 5000
	}

	saved := 0
	for i := 0; i < len(ranges); i += batchSize {
		if ctx.Err() != nil {
			return saved, ctx.Err()
		}

		end := i + batchSize
		if end > len(ranges) {
			end = len(ranges)
		}
		batch := ranges[i:end]

		txErr := g.app.RunInTransaction(func(txApp core.App) error {
			txDB := txApp.NonconcurrentDB()
			if txDB == nil {
				return fmt.Errorf("transaction db is not available")
			}

			query := `INSERT INTO tiger_addr_ranges (full_name, from_hn, to_hn, parity, zip, side, lat, lon)
				VALUES ({:full_name}, {:from_hn}, {:to_hn}, {:parity}, {:zip}, {:side}, {:lat}, {:lon})
				ON CONFLICT(full_name, from_hn, to_hn, side) DO UPDATE SET
					parity = excluded.parity, zip = excluded.zip, lat = excluded.lat, lon = excluded.lon`

			for _, ar := range batch {
				if _, err := txDB.NewQuery(query).Bind(dbx.Params{
					"full_name": ar.FullName,
					"from_hn":   ar.FromHN,
					"to_hn":     ar.ToHN,
					"parity":    ar.Parity,
					"zip":       ar.ZIP,
					"side":      ar.Side,
					"lat":       ar.Lat,
					"lon":       ar.Lon,
				}).Execute(); err != nil {
					return err
				}
			}

			return nil
		})

		if txErr != nil {
			return saved, fmt.Errorf("batch upsert addr ranges failed at offset %d: %w", i, txErr)
		}

		saved += len(batch)
	}

	return saved, nil
}
