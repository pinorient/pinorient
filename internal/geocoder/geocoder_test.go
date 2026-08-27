package geocoder

import (
	"math"
	"strings"
	"testing"

	"github.com/pinorient/pinorient/internal/models"
)

func TestSanitizeQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"42 Maple St", "42 Maple St"},
		{"42 Maple St, Springfield", "42 Maple St Springfield"},
		{"42 Maple St , Springfield , MA", "42 Maple St Springfield MA"},
		{"42 Maple St, Springfield, MA 01103", "42 Maple St Springfield MA 01103"},
		{"42 Maple St; Springfield", "42 Maple St Springfield"},
		{",,,", ""},
		{"42  Maple   St", "42 Maple St"},
	}
	for _, c := range cases {
		if got := sanitizeQuery(c.in); got != c.want {
			t.Errorf("sanitizeQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

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
		{"birchwood rd, capitan", "birchwood rd", "capitan"},
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

func TestFTSTokens(t *testing.T) {
	got := ftsTokens("  Bowdoin  \"College\"  Roux ")
	want := []string{"Bowdoin", "College", "Roux"}
	if len(got) != len(want) {
		t.Fatalf("ftsTokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ftsTokens[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := ftsTokens("   "); len(got) != 0 {
		t.Errorf("ftsTokens(blank) = %v, want empty", got)
	}
}

func TestBuildAndQuery(t *testing.T) {
	got := buildAndQuery([]string{"bowdoin", "college", "a"})
	want := `"bowdoin" AND "college"` // 1-char token skipped
	if got != want {
		t.Errorf("buildAndQuery = %q, want %q", got, want)
	}
	if got := buildAndQuery(nil); got != "" {
		t.Errorf("buildAndQuery(nil) = %q, want empty", got)
	}
}

func TestBuildOrQuery(t *testing.T) {
	cases := []struct {
		name       string
		tokens     []string
		prefixLast bool
		want       string
	}{
		{"exact", []string{"bowdoin", "college", "roux", "center"}, false,
			`"bowdoin" OR "college" OR "roux" OR "center"`},
		{"prefix on short last token", []string{"bowdoin", "college", "cent"}, true,
			`"bowdoin" OR "college" OR cent*`},
		{"no prefix on long last token", []string{"bowdoin", "college", "center"}, true,
			`"bowdoin" OR "college" OR "center"`},
		{"short tokens skipped", []string{"a", "bb", "ccc"}, false, `"ccc"`},
		{"empty", nil, false, ""},
	}
	for _, c := range cases {
		if got := buildOrQuery(c.tokens, c.prefixLast); got != c.want {
			t.Errorf("%s: buildOrQuery(%v, %v) = %q, want %q", c.name, c.tokens, c.prefixLast, got, c.want)
		}
	}
}

func TestPreferMultiTokenMatches(t *testing.T) {
	tokens := []string{"bowdoin", "college", "roux", "center"}
	candidates := []models.Place{
		{Name: "Some Center"}, // single-token match only
		{Name: "Roux Center for the Environment", Address: "44 College Street", City: "Brunswick", State: "ME"}, // 3 hits
		{Name: "Bowdoin College"}, // 2 hits
	}

	got := preferMultiTokenMatches(candidates, tokens, 10)
	if len(got) != 3 {
		t.Fatalf("preferMultiTokenMatches returned %d places, want 3", len(got))
	}
	// Multi-token matches first (preserving their original rank order),
	// single-token matches fill the tail.
	if got[0].Name != "Roux Center for the Environment" {
		t.Errorf("got[0] = %q, want the 3-token match first", got[0].Name)
	}
	if got[1].Name != "Bowdoin College" {
		t.Errorf("got[1] = %q, want the 2-token match second", got[1].Name)
	}
	if got[2].Name != "Some Center" {
		t.Errorf("got[2] = %q, want the single-token match last", got[2].Name)
	}

	// Limit truncation applies after reordering.
	got = preferMultiTokenMatches(candidates, tokens, 1)
	if len(got) != 1 || got[0].Name != "Roux Center for the Environment" {
		t.Errorf("limit=1: got %v, want only the best multi-token match", got)
	}
}

func TestTokenHits(t *testing.T) {
	p := &models.Place{
		Name: "Roux Center for the Environment", Address: "44 College Street",
		City: "Brunswick", State: "ME", Postcode: "04011",
	}
	hits := tokenHits(p, []string{"roux", "center", "college", "bowdoin", "brunswick"})
	if hits != 4 {
		t.Errorf("tokenHits = %d, want 4 (bowdoin must not match)", hits)
	}
	// Case-insensitive, substring (prefix) matching.
	if hits := tokenHits(p, []string{"ROUX", "cent"}); hits != 2 {
		t.Errorf("tokenHits case/prefix = %d, want 2", hits)
	}
}

func TestPickAnchorTokens(t *testing.T) {
	counts := map[string]int64{"roux": 150, "bowdoin": 3443, "college": 85075, "center": 292620}

	// The Bowdoin case: anchor on the two rare tokens only; "college" would
	// blow the tiny extend budget and "center" is far too common.
	anchors, useAND := pickAnchorTokens([]string{"Bowdoin", "College", "Roux", "Center"}, counts)
	if useAND {
		t.Error("rare-token query should use OR, not AND")
	}
	if len(anchors) != 2 || anchors[0] != "Roux" || anchors[1] != "Bowdoin" {
		t.Errorf("anchors = %v, want [Roux Bowdoin] (rarest first)", anchors)
	}

	// Both tokens common: their intersection is the only cheap query.
	anchors, useAND = pickAnchorTokens([]string{"center", "college"}, counts)
	if !useAND {
		t.Error("common-token query should use AND (intersection)")
	}
	if len(anchors) != 2 || anchors[0] != "college" || anchors[1] != "center" {
		t.Errorf("anchors = %v, want [college center] (rarest first)", anchors)
	}

	// All tokens rare: extra anchors join the OR (better recall).
	rare := map[string]int64{"roux": 150, "bowdoin": 3443, "schiller": 40}
	anchors, useAND = pickAnchorTokens([]string{"bowdoin", "roux", "schiller"}, rare)
	if useAND || len(anchors) != 3 {
		t.Errorf("all-rare query: anchors = %v (useAND=%v), want 3 OR anchors", anchors, useAND)
	}

	// A term missing from the index counts as 0 and sorts first.
	anchors, _ = pickAnchorTokens([]string{"bowdin", "roux"}, counts)
	if len(anchors) != 2 || anchors[0] != "bowdin" || anchors[1] != "roux" {
		t.Errorf("missing term: anchors = %v, want [bowdin roux]", anchors)
	}

	// Short tokens are skipped and duplicates removed.
	anchors, _ = pickAnchorTokens([]string{"roux", "ab", "roux"}, counts)
	if len(anchors) != 1 || anchors[0] != "roux" {
		t.Errorf("dedupe/skip: anchors = %v, want [roux]", anchors)
	}

	// Nothing usable at all.
	if anchors, _ := pickAnchorTokens([]string{"a", "bb"}, counts); len(anchors) != 0 {
		t.Errorf("no valid tokens: anchors = %v, want empty", anchors)
	}
}

func TestMinEffectiveCount(t *testing.T) {
	counts := map[string]int64{"roux": 150, "bowdoin": 3443, "main": 436531, "street": 10530716}

	// The rarest token drives intersection cost.
	if got := minEffectiveCount([]string{"Bowdoin", "Street"}, counts, false); got != 3443 {
		t.Errorf("minEffectiveCount = %d, want 3443 (case-insensitive)", got)
	}
	// Terms missing from the index count as 0 → strict matching is free.
	if got := minEffectiveCount([]string{"nothere", "street"}, counts, false); got != 0 {
		t.Errorf("missing term: got %d, want 0", got)
	}
	// Prefix mode weights the final short token (only 3-5 chars).
	if got := minEffectiveCount([]string{"street", "main"}, counts, true); got != 436531*prefixCostMultiplier {
		t.Errorf("prefix weighting: got %d, want %d", got, 436531*prefixCostMultiplier)
	}
	// Long final token is not prefix-expanded, so no weighting.
	if got := minEffectiveCount([]string{"street", "bowdoin"}, counts, true); got != 3443 {
		t.Errorf("no prefix on long token: got %d, want 3443", got)
	}
	// Short tokens are skipped entirely (never probed, never gate drivers).
	if got := minEffectiveCount([]string{"rd", "roux"}, counts, false); got != 150 {
		t.Errorf("short token skip: got %d, want 150", got)
	}
	// No countable tokens at all → treated as expensive (strict tiers skipped).
	if got := minEffectiveCount([]string{"15", "rd"}, counts, false); got != termCountUnknown {
		t.Errorf("all-short tokens: got %d, want termCountUnknown", got)
	}
	if got := minEffectiveCount(nil, counts, false); got != termCountUnknown {
		t.Errorf("empty tokens: got %d, want termCountUnknown", got)
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

func TestReverseBoxes(t *testing.T) {
	boxes := reverseBoxes(0)
	if len(boxes) != 4 {
		t.Fatalf("expected 4 boxes, got %d", len(boxes))
	}
	// At the equator the box is square: dLat == dLon, ~0.5km for the first.
	if math.Abs(boxes[0][0]-0.5/111.0) > 1e-9 || math.Abs(boxes[0][0]-boxes[0][1]) > 1e-12 {
		t.Errorf("equator box should be square ~0.5km, got %v", boxes[0])
	}
	// Boxes must be strictly widening.
	for i := 1; i < len(boxes); i++ {
		if boxes[i][0] <= boxes[i-1][0] {
			t.Errorf("boxes not widening at %d: %v", i, boxes)
		}
	}
	// Near the poles the lon delta widens but stays bounded by the cos floor.
	pole := reverseBoxes(89)
	if pole[0][1] <= boxes[0][1] {
		t.Errorf("expected wider lon delta near poles, got %v vs %v", pole[0][1], boxes[0][1])
	}
	if pole[0][1] > 0.5/(111.0*0.1)+1e-9 {
		t.Errorf("lon delta should be capped by cos floor 0.1, got %v", pole[0][1])
	}
}
