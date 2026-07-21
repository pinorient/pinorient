package geocoder

import (
	"strings"
	"testing"
)

func TestBuildMultiValueInsert(t *testing.T) {
	rows := [][]any{
		{"node/1", int64(1), "Main St"},
		{"node/2", int64(2), "Elm St"},
	}

	sql, params := buildMultiValueInsert(
		"INSERT INTO t (id, osm_id, name) VALUES ",
		", datetime('now')",
		" ON CONFLICT(id) DO NOTHING",
		rows)

	want := "INSERT INTO t (id, osm_id, name) VALUES " +
		"({:v0_0},{:v0_1},{:v0_2}, datetime('now'))," +
		"({:v1_0},{:v1_1},{:v1_2}, datetime('now'))" +
		" ON CONFLICT(id) DO NOTHING"
	if sql != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", sql, want)
	}

	if len(params) != 6 {
		t.Errorf("len(params) = %d, want 6", len(params))
	}
	if params["v0_0"] != "node/1" || params["v1_2"] != "Elm St" {
		t.Errorf("param values wrong: %v", params)
	}
}

// TestMultiValueInsertStaysUnderVariableLimit verifies that a maximal chunk
// for the widest table we bulk-insert (geocoder_places, 14 bound columns)
// stays below SQLite's historical bound-variable limit of 999.
func TestMultiValueInsertStaysUnderVariableLimit(t *testing.T) {
	const placesCols = 14
	perStmt := maxInsertVars / placesCols

	rows := make([][]any, perStmt)
	for i := range rows {
		row := make([]any, placesCols)
		for c := range row {
			row[c] = c
		}
		rows[i] = row
	}

	sql, params := buildMultiValueInsert("INSERT INTO t VALUES ", "", "", rows)
	if len(params) > 999 {
		t.Errorf("statement binds %d variables, over SQLite's 999 limit", len(params))
	}
	if got, want := strings.Count(sql, "{:"), len(params); got != want {
		t.Errorf("SQL has %d bind sites but %d params", got, want)
	}
}
