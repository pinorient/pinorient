package db

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// RunMigrations creates the SQLite tables and FTS5 index used by the geocoder.
func RunMigrations(app core.App) error {
	db := app.DB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	queries := []string{
		`CREATE TABLE IF NOT EXISTS geocoder_places (
			id TEXT PRIMARY KEY,
			osm_id INTEGER NOT NULL,
			osm_type TEXT NOT NULL,
			name TEXT NOT NULL,
			address TEXT,
			city TEXT,
			state TEXT,
			postcode TEXT,
			country TEXT DEFAULT 'US',
			lat REAL NOT NULL,
			lon REAL NOT NULL,
			class TEXT,
			type TEXT,
			importance REAL,
			created TEXT DEFAULT (datetime('now')),
			updated TEXT DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_places_osm_id ON geocoder_places(osm_id)`,
		`CREATE INDEX IF NOT EXISTS idx_places_name ON geocoder_places(name)`,
		`CREATE INDEX IF NOT EXISTS idx_places_city ON geocoder_places(city)`,
		`CREATE INDEX IF NOT EXISTS idx_places_state ON geocoder_places(state)`,
		`CREATE INDEX IF NOT EXISTS idx_places_postcode ON geocoder_places(postcode)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS geocoder_places_fts USING fts5(
			name, address, city, state, postcode,
			content='geocoder_places',
			content_rowid='id'
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

	for _, q := range queries {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	return nil
}
