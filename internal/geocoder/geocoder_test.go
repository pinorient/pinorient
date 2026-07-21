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

func TestSplitStreetContext(t *testing.T) {
	cases := []struct {
		in, street, context string
	}{
		{"birchwood rd capitan", "birchwood rd", "capitan"},
		{"birchwood rd capitan nm", "birchwood rd", "capitan nm"},
		{"birchwood rd", "birchwood rd", ""},
		{"main st springfield", "main st", "springfield"},
		{"mount vernon st boston", "mount vernon st", "boston"},
		{"birchwood park dr capitan", "birchwood park dr", "capitan"},
		{"birchwood road capitan", "birchwood road", "capitan"},
		{"birchwood road capitan nm", "birchwood road", "capitan nm"},
		{"main street springfield", "main street", "springfield"},
		{"roger williams avenue providence", "roger williams avenue", "providence"},
		{"blue hills parkway milton", "blue hills parkway", "milton"},
		{"annapolis pike bowie", "annapolis pike", "bowie"},
		{"old colony turnpike burlington", "old colony turnpike", "burlington"},
		{"fifth street", "fifth street", ""},
		{"birchwood road", "birchwood road", ""},
		{"st marys rd", "st marys rd", ""}, // "St" = Saint, not a suffix here
		{"broadway", "broadway", ""},
		{"capitan rd", "capitan rd", ""},
		{"maple street", "maple street", ""},
		{"park ave new york", "park ave", "new york"},
	}
	for _, c := range cases {
		street, context := splitStreetContext(c.in)
		if street != c.street || context != c.context {
			t.Errorf("splitStreetContext(%q) = (%q, %q), want (%q, %q)", c.in, street, context, c.street, c.context)
		}
	}
}

func TestSplitContextState(t *testing.T) {
	cases := []struct {
		in, city, state string
	}{
		{"capitan nm", "capitan", "NM"},
		{"capitan", "capitan", ""},
		{"ma", "", "MA"},
		{"kansas city mo", "kansas city", "MO"},
		{"nowhereville", "nowhereville", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		city, state := splitContextState(c.in)
		if city != c.city || state != c.state {
			t.Errorf("splitContextState(%q) = (%q, %q), want (%q, %q)", c.in, city, state, c.city, c.state)
		}
	}
}

func TestNormalizeStreetName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Birchwood Road", "Birchwood Rd"},
		{"birchwood road", "birchwood Rd"},
		{"Main Street", "Main St"},
		{"Blue Hills Parkway", "Blue Hills Pkwy"},
		{"Veterans Memorial Highway", "Veterans Memorial Hwy"},
		{"Oak Terrace", "Oak Ter"},
		{"Appalachian Trail", "Appalachian Trl"},
		{"Harvard Square", "Harvard Sq"},
		{"Dowling Crossing", "Dowling Xing"},
		{"Old Colony Turnpike", "Old Colony Tpke"},
		{"Birchwood Park Dr", "Birchwood Park Dr"},   // "Park" must NOT be replaced
		{"Birchwood Ridge Pl", "Birchwood Ridge Pl"}, // "Ridge" must NOT be replaced
	}
	for _, c := range cases {
		if got := normalizeStreetName(c.in); got != c.want {
			t.Errorf("normalizeStreetName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
