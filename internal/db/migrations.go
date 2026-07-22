package db

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"os"
	"time"

	"github.com/pocketbase/dbx"
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

	// Set SQLite performance pragmas. Cache size and mmap size are configurable
	// via environment variables to support servers with different amounts of RAM.
	cacheSize := os.Getenv("DB_CACHE_SIZE")
	if cacheSize == "" {
		cacheSize = "65536" // 64MB (safe for 2GB servers)
	}
	mmapSize := os.Getenv("DB_MMAP_SIZE")
	if mmapSize == "" {
		mmapSize = "0" // disabled (safe for 2GB servers)
	}
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-" + cacheSize,
		"PRAGMA mmap_size=" + mmapSize,
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
			UNIQUE(full_name, from_hn, to_hn, side, zip)
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

	// 5b. Fix the tiger_addr_ranges conflict key on existing databases: the
	// original UNIQUE constraint did not include zip, so identical address
	// ranges in different ZIPs (same street name, house number range, and
	// side) collapsed into one row whose ZIP depended on import order.
	if err := migrateTigerZipConflictKey(app); err != nil {
		return fmt.Errorf("tiger conflict key migration failed: %w", err)
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

	// 7. Create the _import_state table for tracking import progress.
	//    This enables crash recovery: if an import is interrupted, the scheduler
	//    can resume from where it left off instead of starting over.
	//    Used primarily for TIGER county-by-county resume tracking.
	_, err = db.NewQuery(`CREATE TABLE IF NOT EXISTS _import_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`).Execute()
	if err != nil {
		return fmt.Errorf("import state migration failed: %w", err)
	}

	return nil
}

// migrateTigerZipConflictKey rebuilds tiger_addr_ranges with the zip-aware
// UNIQUE constraint when the table still has the legacy constraint.
//
// SQLite cannot alter constraints (and auto-indexes backing UNIQUE constraints
// can't be dropped), so the table is rebuilt: rows are copied in committed
// rowid-ordered chunks (preserving rowids so the external-content FTS stays
// valid until its rebuild), progress is persisted per chunk so an interrupted
// migration resumes where it stopped, and the swap is atomic.
//
// Note: the copy preserves whatever rows exist, but rows previously LOST to
// conflict-key collisions can only be restored by re-importing the affected
// counties (delete their tiger_county_* markers after upgrading).
func migrateTigerZipConflictKey(app core.App) error {
	db := app.DB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	var tableSQL string
	_ = db.NewQuery("SELECT sql FROM sqlite_master WHERE type='table' AND name='tiger_addr_ranges'").Row(&tableSQL)
	if tableSQL == "" || strings.Contains(tableSQL, "UNIQUE(full_name, from_hn, to_hn, side, zip)") {
		return nil // Fresh install (new schema) or already migrated.
	}

	// _import_state is created later in RunMigrations on first installs; the
	// migration needs it now for resume tracking.
	if _, err := db.NewQuery(`CREATE TABLE IF NOT EXISTS _import_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`).Execute(); err != nil {
		return fmt.Errorf("failed to ensure import state table: %w", err)
	}

	const offsetKey = "tiger_zip_migration_offset"
	var after int64
	var offset string
	_ = db.NewQuery("SELECT value FROM _import_state WHERE key = {:key}").Bind(dbx.Params{"key": offsetKey}).Row(&offset)
	if offset != "" {
		after, _ = strconv.ParseInt(offset, 10, 64)
		log.Printf("resuming tiger_addr_ranges conflict key migration from rowid %d", after)
	} else {
		log.Printf("migrating tiger_addr_ranges to zip-aware conflict key (one-time table rebuild)...")
		setup := []string{
			"DROP TABLE IF EXISTS tiger_addr_ranges_new",
			`CREATE TABLE tiger_addr_ranges_new (
				full_name TEXT NOT NULL,
				from_hn INTEGER NOT NULL,
				to_hn INTEGER NOT NULL,
				parity TEXT NOT NULL,
				zip TEXT,
				side TEXT NOT NULL,
				lat REAL NOT NULL,
				lon REAL NOT NULL,
				UNIQUE(full_name, from_hn, to_hn, side, zip)
			)`,
		}
		for _, q := range setup {
			if _, err := db.NewQuery(q).Execute(); err != nil {
				return fmt.Errorf("failed to create replacement table: %w", err)
			}
		}
	}

	const chunkSize = 250000
	copyInsert := `INSERT INTO tiger_addr_ranges_new
		(rowid, full_name, from_hn, to_hn, parity, zip, side, lat, lon)
		SELECT rowid, full_name, from_hn, to_hn, parity, zip, side, lat, lon
		FROM tiger_addr_ranges WHERE rowid > {:after} AND rowid <= {:upto}`

	startTime := time.Now()
	for {
		var upto, count int64
		if err := db.NewQuery(
			"SELECT COALESCE(MAX(rowid), 0), COUNT(*) FROM (SELECT rowid FROM tiger_addr_ranges WHERE rowid > {:after} ORDER BY rowid LIMIT {:n})",
		).Bind(dbx.Params{"after": after, "n": chunkSize}).Row(&upto, &count); err != nil {
			return fmt.Errorf("failed to scan tiger_addr_ranges: %w", err)
		}
		if count == 0 {
			break
		}

		txErr := app.RunInTransaction(func(txApp core.App) error {
			txDB := txApp.NonconcurrentDB()
			if txDB == nil {
				return fmt.Errorf("transaction db is not available")
			}
			if _, err := txDB.NewQuery(copyInsert).Bind(dbx.Params{"after": after, "upto": upto}).Execute(); err != nil {
				return err
			}
			// Persist progress in the same transaction so a crash resumes
			// exactly at the last committed chunk.
			_, err := txDB.NewQuery(`INSERT INTO _import_state (key, value) VALUES ({:key}, {:value})
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`).
				Bind(dbx.Params{"key": offsetKey, "value": strconv.FormatInt(upto, 10)}).Execute()
			return err
		})
		if txErr != nil {
			return fmt.Errorf("migration copy failed at rowid %d: %w", upto, txErr)
		}

		after = upto
		log.Printf("tiger_addr_ranges migration progress: rowid %d", after)
	}

	// Swap atomically (and clear the resume marker in the same transaction).
	txErr := app.RunInTransaction(func(txApp core.App) error {
		txDB := txApp.NonconcurrentDB()
		if txDB == nil {
			return fmt.Errorf("transaction db is not available")
		}
		stmts := []string{
			"DROP TABLE tiger_addr_ranges",
			"ALTER TABLE tiger_addr_ranges_new RENAME TO tiger_addr_ranges",
			"CREATE INDEX IF NOT EXISTS idx_tiger_addr_full_name ON tiger_addr_ranges(full_name)",
			"CREATE INDEX IF NOT EXISTS idx_tiger_addr_hn ON tiger_addr_ranges(from_hn, to_hn)",
			"DELETE FROM _import_state WHERE key = '" + offsetKey + "'",
		}
		for _, q := range stmts {
			if _, err := txDB.NewQuery(q).Execute(); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		return fmt.Errorf("failed to swap rebuilt tiger_addr_ranges: %w", txErr)
	}

	// The FTS index was built under the legacy conflict key (and is missing
	// the rows lost to collisions): force a rebuild on the next scheduler pass.
	if _, err := db.NewQuery("DELETE FROM _import_state WHERE key IN ('tiger_fts_done', 'tiger_fts_offset')").Execute(); err != nil {
		log.Printf("warning: failed to clear TIGER FTS state after migration: %v", err)
	}

	log.Printf("tiger_addr_ranges conflict key migration complete in %s", time.Since(startTime).Round(time.Second))
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
