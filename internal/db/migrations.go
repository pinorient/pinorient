package db

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// RunMigrations creates the PocketBase collection and FTS5 index used by the geocoder.
func RunMigrations(app core.App) error {
	db := app.DB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	// 1. Check if the FTS5 table already exists.
	//    Only drop and recreate if it doesn't exist (first run or schema change).
	//    Dropping on every startup would destroy the index and force a hours-long rebuild.
	var ftsTableExists int
	_ = db.NewQuery("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='geocoder_places_fts'").Row(&ftsTableExists)

	if ftsTableExists == 0 {
		// FTS table doesn't exist — drop any stale triggers and create fresh.
		ftsCleanup := []string{
			"DROP TRIGGER IF EXISTS places_fts_insert",
			"DROP TRIGGER IF EXISTS places_fts_delete",
			"DROP TRIGGER IF EXISTS places_fts_update",
			"DROP TABLE IF EXISTS geocoder_places_fts",
		}
		for _, q := range ftsCleanup {
			if _, err := db.NewQuery(q).Execute(); err != nil {
				return fmt.Errorf("fts cleanup failed: %w", err)
			}
		}
	}

	// 2. Create or ensure the geocoder_places PocketBase collection exists.
	collection, err := app.FindCollectionByNameOrId("geocoder_places")
	if err != nil {
		// Collection doesn't exist. Check if a legacy raw SQL table exists.
		var tableExists int
		_ = db.NewQuery("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='geocoder_places'").Row(&tableExists)
		if tableExists > 0 {
			// Drop the legacy table so PocketBase can create a fresh one with the correct schema.
			if _, err := db.NewQuery("DROP TABLE geocoder_places").Execute(); err != nil {
				return fmt.Errorf("failed to drop legacy geocoder_places table: %w", err)
			}
		}

		// Create the PocketBase collection.
		collection = createGeocoderPlacesCollection()
		if err := app.Save(collection); err != nil {
			return fmt.Errorf("failed to create geocoder_places collection: %w", err)
		}
	}

	// 3. Create the FTS5 virtual table and sync triggers (IF NOT EXISTS so safe to run always).
	//    Use the default rowid (not content_rowid='id') since id is TEXT.
	//    The triggers use new.rowid/old.rowid which are the implicit integer rowids.
	ftsQueries := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS geocoder_places_fts USING fts5(
			name, address, city, state, postcode,
			content='geocoder_places'
		)`,
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
	for _, q := range ftsQueries {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("fts migration failed: %w", err)
		}
	}

	// Set SQLite performance pragmas for the 16GB database.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-262144",
		"PRAGMA mmap_size=4294967296",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.NewQuery(p).Execute(); err != nil {
			app.Logger().Warn("failed to set pragma", "pragma", p, "error", err)
		}
	}

	// 4. FTS index rebuild is handled asynchronously by the application
	//    after the server starts (see main.go). This prevents a hours-long
	//    rebuild from blocking server startup.

	// 5. Create the TIGER/Line address ranges table for address interpolation.
	//    This is a raw SQL table (not a PocketBase collection) because it's
	//    used internally by the geocoder for interpolation, not exposed via admin UI.
	tigerQueries := []string{
		`CREATE TABLE IF NOT EXISTS tiger_addr_ranges (
			full_name TEXT NOT NULL,
			from_hn INTEGER NOT NULL,
			to_hn INTEGER NOT NULL,
			parity TEXT NOT NULL,
			zip TEXT,
			side TEXT NOT NULL,
			lat REAL NOT NULL,
			lon REAL NOT NULL,
			UNIQUE(full_name, from_hn, to_hn, side)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tiger_addr_full_name ON tiger_addr_ranges(full_name)`,
		`CREATE INDEX IF NOT EXISTS idx_tiger_addr_hn ON tiger_addr_ranges(from_hn, to_hn)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS tiger_addr_fts USING fts5(
			full_name,
			content='tiger_addr_ranges'
		)`,
	}
	for _, q := range tigerQueries {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("tiger migration failed: %w", err)
		}
	}

	// 6. Create the zip_city_state cache table (empty; populated by scheduler).
	//    This table maps ZIP codes to their most common city/state from OSM data,
	//    enabling fast JOINs in TIGER address interpolation instead of N+1 lookups.
	zipCacheQueries := []string{
		`CREATE TABLE IF NOT EXISTS zip_city_state (
			postcode TEXT NOT NULL,
			city TEXT,
			state TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_zip_city_state ON zip_city_state(postcode)`,
	}
	for _, q := range zipCacheQueries {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("zip cache migration failed: %w", err)
		}
	}

	return nil
}

// createGeocoderPlacesCollection builds the PocketBase collection definition for geocoder places.
// This makes the data viewable and manageable from the PocketBase admin UI.
func createGeocoderPlacesCollection() *core.Collection {
	c := core.NewBaseCollection("geocoder_places")

	c.Fields.Add(
		&core.NumberField{Name: "osm_id", OnlyInt: true, Help: "OpenStreetMap element ID"},
		&core.TextField{Name: "osm_type", Help: "OSM element type: node, way, or relation"},
		&core.TextField{Name: "name", Required: true, Presentable: true, Help: "Place name"},
		&core.TextField{Name: "address"},
		&core.TextField{Name: "city"},
		&core.TextField{Name: "state"},
		&core.TextField{Name: "postcode"},
		&core.TextField{Name: "country"},
		&core.NumberField{Name: "lat", Help: "Latitude (0 for ways without coordinates)"},
		&core.NumberField{Name: "lon", Help: "Longitude (0 for ways without coordinates)"},
		&core.TextField{Name: "class", Help: "OSM class category"},
		&core.TextField{Name: "type", Help: "OSM type within class"},
		&core.NumberField{Name: "importance", Help: "Search importance ranking"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)

	c.AddIndex("idx_places_osm_id", false, "osm_id", "")
	c.AddIndex("idx_places_name", false, "name", "")
	c.AddIndex("idx_places_city", false, "city", "")
	c.AddIndex("idx_places_state", false, "state", "")
	c.AddIndex("idx_places_postcode", false, "postcode", "")

	return c
}
