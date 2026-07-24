package geocoder

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/pinorient/pinorient/internal/config"
	"github.com/pinorient/pinorient/internal/models"
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

	// termCounts caches per-term FTS document counts (used only for query
	// cost estimation — a stale value just misestimates cost, never
	// correctness). Bounded by termCountsCacheCap.
	termCounts   map[string]int64
	termCountsMu sync.RWMutex
}

// New creates a new Geocoder instance.
func New(app core.App, cfg *config.Config) *Geocoder {
	return &Geocoder{app: app, cfg: cfg, termCounts: make(map[string]int64)}
}

// Import-state keys used in the _import_state table to make long-running
// imports crash-resumable. A missing "*_done" marker means the corresponding
// step never completed and must be re-run (or resumed) on startup.
const (
	// StateOSMPlacesDone is set when a full OSM row import completes.
	StateOSMPlacesDone = "osm_places_done"
	// StateOSMFTSDone is set when the places FTS5 index is fully rebuilt.
	StateOSMFTSDone = "osm_fts_done"
	// StateOSMFTSOffset holds the last committed rowid during a chunked
	// places FTS rebuild (deleted on completion).
	StateOSMFTSOffset = "osm_fts_offset"
	// StateTigerFTSDone is set when the TIGER FTS5 index is fully rebuilt.
	StateTigerFTSDone = "tiger_fts_done"
	// StateTigerFTSOffset holds the last committed rowid during a chunked
	// TIGER FTS rebuild (deleted on completion).
	StateTigerFTSOffset = "tiger_fts_offset"
	// StateOSMNodeCoordsDone is set when the coordinates of all way-referenced
	// nodes have been collected into the _osm_node_coords table.
	StateOSMNodeCoordsDone = "osm_nodecoords_done"
	// StateOSMWayCoordsDone is set when every indexed way has been assigned a
	// centroid from its member nodes.
	StateOSMWayCoordsDone = "osm_waycoords_done"
	// StateOSMWayCoordsOffset holds the last committed way OSM ID during the
	// chunked way-centroid pass (deleted on completion).
	StateOSMWayCoordsOffset = "osm_waycoords_offset"
)

// maxInsertVars caps the number of bound variables per INSERT statement.
// SQLite's historical SQLITE_MAX_VARIABLE_NUMBER is 999 (32766 in modern
// builds); staying below it keeps the generated SQL portable.
const maxInsertVars = 900

// defaultFTSRebuildChunkSize is the fallback chunk size for chunked FTS
// rebuilds when none is configured.
const defaultFTSRebuildChunkSize = 250000

// execMultiValueInsert executes an INSERT with multiple VALUES rows per
// statement, chunking the rows so no statement binds more than maxInsertVars
// variables. Bulk imports previously parsed one INSERT per row (tens of
// millions of parses for the full US dataset); this is dramatically faster.
//
//	head:      "INSERT INTO tbl (c1, c2) VALUES " — column count must equal len(row) (+ literals in rowSuffix).
//	rowSuffix: literal SQL appended inside each row's parentheses after the
//	           bound values, e.g. ", datetime('now')" (may be empty).
//	tail:      SQL appended after the VALUES list (e.g. an ON CONFLICT clause).
//	rows:      one slice of bound values per row, all the same length.
func execMultiValueInsert(txDB dbx.Builder, head, rowSuffix, tail string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	cols := len(rows[0])
	if cols == 0 {
		return fmt.Errorf("insert rows must have at least one column")
	}
	perStmt := maxInsertVars / cols
	if perStmt < 1 {
		perStmt = 1
	}

	for start := 0; start < len(rows); start += perStmt {
		end := start + perStmt
		if end > len(rows) {
			end = len(rows)
		}
		sql, params := buildMultiValueInsert(head, rowSuffix, tail, rows[start:end])
		if _, err := txDB.NewQuery(sql).Bind(params).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// buildMultiValueInsert builds a single multi-row INSERT statement with named
// bind parameters (see execMultiValueInsert). Extracted as a pure function
// for testability.
func buildMultiValueInsert(head, rowSuffix, tail string, rows [][]any) (string, dbx.Params) {
	var sb strings.Builder
	sb.WriteString(head)
	params := make(dbx.Params, len(rows)*len(rows[0]))
	for r, row := range rows {
		if r > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for c, val := range row {
			if c > 0 {
				sb.WriteByte(',')
			}
			key := fmt.Sprintf("v%d_%d", r, c)
			sb.WriteString("{:" + key + "}")
			params[key] = val
		}
		sb.WriteString(rowSuffix)
		sb.WriteByte(')')
	}
	sb.WriteString(tail)
	return sb.String(), params
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
		tigerPlaces, err := g.InterpolateAddress(ctx, houseNum, streetName, limit, bbox)
		if err != nil {
			// Interpolation is best-effort; log and continue with OSM results.
			g.app.Logger().Warn("tiger interpolation failed", "error", err, "query", q)
		}
		places = append(places, tigerPlaces...)
	}

	// Use FTS5 to match the query across indexed fields. The plan is
	// cost-aware: FTS5 intersections are driven by the rarest token's doclist,
	// so a cheap vocab count lookup tells us whether strict matching (phrase
	// / tokenized AND) is affordable or whether to go straight to the
	// anchored partial match. Naive strict queries over common tokens
	// ("street", "center") can take seconds on the full dataset.
	const orderBy = "bm25(geocoder_places_fts, 10.0, 1.0, 1.0, 1.0, 1.0)"
	tokens := ftsTokens(q)
	counts, countsErr := g.cachedTermDocCounts(ctx, tokens)

	// The raw query as a phrase match (all tokens adjacent, in order) is the
	// highest-precision interpretation. Run it only when estimated cheap.
	var osmPlaces []models.Place
	var err error
	if countsErr != nil || len(tokens) < 2 || minEffectiveCount(tokens, counts, false) <= phraseMinDocBudget {
		osmPlaces, err = g.ftsPlaces(ctx, q, limit, bbox, orderBy)
		if err != nil {
			return nil, fmt.Errorf("search failed: %w", err)
		}
	}
	if len(osmPlaces) == 0 {
		andOK := countsErr != nil || minEffectiveCount(tokens, counts, false) <= andMinDocBudget
		osmPlaces, err = g.fallbackPartialMatch(ctx, tokens, counts, countsErr, limit, bbox, orderBy, false, andOK)
		if err != nil {
			return nil, fmt.Errorf("search failed: %w", err)
		}
	}

	places = append(places, osmPlaces...)
	if len(places) > limit {
		places = places[:limit]
	}

	g.enrichFromZipCache(ctx, places)
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

	// Cost-aware planning as in Search: run the strict prefix AND only when
	// the rarest token's doclist is small enough to intersect cheaply.
	tokens := ftsTokens(q)
	counts, countsErr := g.cachedTermDocCounts(ctx, tokens)
	andOK := countsErr != nil || len(tokens) < 2 || minEffectiveCount(tokens, counts, true) <= andMinDocBudget

	var places []models.Place
	var err error
	if andOK {
		// Build FTS5 prefix query: "1600 pen*" -> "1600* pen*"
		ftsQuery := buildPrefixQuery(q)
		if ftsQuery == "" {
			return []models.Place{}, nil
		}
		places, err = g.ftsPlaces(ctx, ftsQuery, limit, bbox, "rank")
		if err != nil {
			return nil, fmt.Errorf("autocomplete failed: %w", err)
		}
	}
	if len(places) == 0 {
		// The strict all-token prefix query matched nothing (e.g. an
		// institution + building name spanning multiple OSM features) or was
		// estimated too expensive. Fall back to the anchored partial match.
		places, err = g.fallbackPartialMatch(ctx, tokens, counts, countsErr, limit, bbox,
			"bm25(geocoder_places_fts, 10.0, 1.0, 1.0, 1.0, 1.0)", true, false)
		if err != nil {
			return nil, fmt.Errorf("autocomplete failed: %w", err)
		}
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
		tigerPlaces, err := g.InterpolateAddress(ctx, houseNum, streetName, maxTiger, bbox)
		if err != nil {
			// Interpolation is best-effort; log and continue with OSM results.
			g.app.Logger().Warn("tiger interpolation failed", "error", err, "query", q)
		}
		places = append(tigerPlaces, places...)
	}
	if len(places) > limit {
		places = places[:limit]
	}

	g.enrichFromZipCache(ctx, places)
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

const (
	// maxFallbackTokens caps how many query tokens the partial-match fallback
	// considers. Beyond this the OR query becomes too broad to be useful.
	maxFallbackTokens = 8
	// orFetchFactor over-fetches OR-fallback candidates relative to the
	// requested limit so the multi-token preference has rows to work with.
	orFetchFactor = 4
	// orFetchMin/orFetchMax bound the OR-fallback candidate fetch size. The
	// pool must be large because bm25 scores candidates on the anchor terms
	// only — the best multi-token matches can sit deep in the anchor order
	// (e.g. a short name like "Roux" outranks "Roux Center ..." on the term
	// "roux"). Evaluating the candidate set costs the same regardless of
	// LIMIT, so a generous pool is nearly free.
	orFetchMin = 200
	orFetchMax = 500
	// anchorANDRankBudget bounds the result-set size for which an anchored
	// AND intersection is bm25-ranked. Ranking costs O(result set): when even
	// the rarest anchor is huge ("main" AND "street" = ~400K rows), ranking
	// takes seconds while an unordered LIMIT returns instantly — and for such
	// degenerate queries any matching rows are equally (un)informative.
	anchorANDRankBudget = 200000
	// anchorDocThreshold bounds how many documents the anchored OR candidate
	// query may scan. The two rarest tokens are always used; if even those
	// exceed the threshold they are AND-ed (intersection) instead.
	anchorDocThreshold = 100000
	// anchorExtendBudget caps cumulative document counts when adding a 3rd
	// or 4th anchor token: extra anchors are only worth it when every token
	// is rare — otherwise common-token matches just dilute the candidates.
	anchorExtendBudget = 10000
	// phraseMinDocBudget is the maximum rarest-token document count for which
	// a phrase query is considered cheap. Phrase matching verifies token
	// positions across the doclist (~8x pricier per row than a plain AND):
	// 25K docs ≈ a few hundred ms on a small server.
	phraseMinDocBudget = 25000
	// andMinDocBudget is the maximum rarest-token document count for which a
	// multi-token AND is considered cheap (FTS5 intersects by probing other
	// doclists from the smallest one).
	andMinDocBudget = 50000
	// prefixCostMultiplier weights an autocomplete prefix token in cost
	// estimates: a prefix expands to several index terms.
	prefixCostMultiplier = 5
	// termCountsCacheCap bounds the term document-count cache. On overflow it
	// is reset wholesale (counts are only estimates; a reset just means a few
	// extra vocab lookups).
	termCountsCacheCap = 50000
)

// ftsPlaces runs a single FTS5 query against geocoder_places_fts and returns
// up to limit places, optionally filtered to bbox. orderBy is an internal
// constant SQL expression ("rank" or a bm25(...) call), never user input.
func (g *Geocoder) ftsPlaces(ctx context.Context, ftsQuery string, limit int, bbox *BBox, orderBy string) ([]models.Place, error) {
	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
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

	// An empty orderBy omits the ORDER BY entirely: FTS5 then returns rows in
	// index order and LIMIT stops the scan early, which is what makes the
	// unranked degenerate-query path fast.
	orderClause := ""
	if orderBy != "" {
		orderClause = "ORDER BY " + orderBy
	}

	query := fmt.Sprintf(`
		SELECT p.id, p.osm_id, p.osm_type, p.name, p.address, p.city, p.state,
		       p.postcode, p.country, p.lat, p.lon, p.class, p.type, p.importance,
		       p.created, p.updated
		FROM geocoder_places p
		INNER JOIN geocoder_places_fts f ON p.rowid = f.rowid
		WHERE geocoder_places_fts MATCH {:query}
		%s
		%s
		LIMIT {:limit}
	`, bboxClause, orderClause)

	var places []models.Place
	if err := db.
		NewQuery(query).
		Bind(params).
		All(&places); err != nil {
		return nil, err
	}
	return places, nil
}

// fallbackPartialMatch runs progressively looser FTS5 matching for queries
// whose strict interpretation matched nothing. Real-world place queries often
// span multiple OSM features ("Bowdoin College Roux Center" = the campus named
// "Bowdoin College" plus the building named "Roux Center ..."), so requiring
// every token in one record returns nothing at all. Photon/Nominatim handle
// this with ranked partial matching; this is the SQLite equivalent:
//
//  1. tokenized AND — every token must match, but in any field and any order
//     (skipped for prefix matching, where the caller already tried an AND);
//  2. anchored ranked OR — a plain OR of every token forces FTS5 to visit
//     and rank millions of rows for common terms ("center", "street"), which
//     took ~10s on the production dataset. Instead the OR is anchored on the
//     rarest tokens (per the index's own document frequencies), and
//     preferMultiTokenMatches ranks rows matching several tokens ahead of
//     single-token noise.
//
// prefixLast enables autocomplete-style prefix matching on the final token
// (only for the legacy unanchored path; anchored candidates rely on the
// substring-aware token-hit preference instead). andBudgetOK reports whether
// the caller's cost estimate allows the exact-token AND tier. countsErr
// indicates the document-count lookup failed; the function then degrades to
// the legacy unanchored OR rather than failing the request.
func (g *Geocoder) fallbackPartialMatch(ctx context.Context, tokens []string, counts map[string]int64, countsErr error, limit int, bbox *BBox, orderBy string, prefixLast, andBudgetOK bool) ([]models.Place, error) {
	if len(tokens) < 2 || len(tokens) > maxFallbackTokens {
		// A single token gains nothing from an OR (identical to the AND),
		// and very long queries make the OR too broad to be meaningful.
		return nil, nil
	}

	if !prefixLast && andBudgetOK {
		if andQuery := buildAndQuery(tokens); andQuery != "" {
			places, err := g.ftsPlaces(ctx, andQuery, limit, bbox, orderBy)
			if err != nil {
				return nil, err
			}
			if len(places) > 0 {
				return places, nil
			}
		}
	}

	if countsErr != nil {
		// Degrade to the legacy unanchored OR rather than failing the
		// request (e.g. on a SQLite build without fts5vocab).
		log.Printf("warning: term doc counts unavailable (%v); using unanchored OR fallback", countsErr)
		return g.orCandidates(ctx, buildOrQuery(tokens, prefixLast), orderBy, tokens, limit, bbox)
	}

	// Anchor the candidate query on the rarest tokens using the FTS index's
	// own per-term document counts (fast index lookups via fts5vocab).
	anchors, useAND := pickAnchorTokens(tokens, counts)

	var candQuery, candOrderBy string
	if useAND {
		candQuery = buildAndQuery(anchors)
		candOrderBy = orderBy
		if minEffectiveCount(anchors, counts, false) > anchorANDRankBudget {
			// The intersection is enormous — skip the bm25 ranking (it costs
			// seconds) and take the first matching rows instead. Ordering by
			// anything (even p.rowid) would force a plan scan; no ORDER BY
			// lets FTS5 stop at LIMIT.
			candOrderBy = ""
		}
	} else {
		candQuery = buildOrQuery(anchors, false)
		candOrderBy = orderBy
	}
	return g.orCandidates(ctx, candQuery, candOrderBy, tokens, limit, bbox)
}

// orCandidates fetches OR-fallback candidates and applies the multi-token
// preference. candQuery is an FTS5 MATCH expression and candOrderBy an
// internal ORDER BY expression, both built by the caller.
func (g *Geocoder) orCandidates(ctx context.Context, candQuery, candOrderBy string, tokens []string, limit int, bbox *BBox) ([]models.Place, error) {
	if candQuery == "" {
		return nil, nil
	}
	fetch := limit * orFetchFactor
	if fetch < orFetchMin {
		fetch = orFetchMin
	}
	if fetch > orFetchMax {
		fetch = orFetchMax
	}
	start := time.Now()
	candidates, err := g.ftsPlaces(ctx, candQuery, fetch, bbox, candOrderBy)
	if err != nil {
		return nil, err
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		log.Printf("slow fallback match: %q took %s", candQuery, elapsed.Round(time.Millisecond))
	}
	return preferMultiTokenMatches(candidates, tokens, limit), nil
}

// termDocCounts returns the number of documents containing each token
// (case-insensitive) according to the FTS5 index. The fts5vocab virtual
// table is a view over the existing index — creating it stores nothing and
// per-term lookups are index probes, not scans. Terms absent from the index
// are missing from the result (callers read them as 0 via the map).
func (g *Geocoder) termDocCounts(ctx context.Context, tokens []string) (map[string]int64, error) {
	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}
	if _, err := db.NewQuery(
		`CREATE VIRTUAL TABLE IF NOT EXISTS geocoder_places_fts_vocab USING fts5vocab('geocoder_places_fts', 'row')`,
	).Execute(); err != nil {
		return nil, fmt.Errorf("failed to create fts vocab table: %w", err)
	}

	// Dedupe and lowercase (the FTS unicode61 tokenizer lowercases terms).
	seen := make(map[string]bool, len(tokens))
	terms := make([]string, 0, len(tokens))
	for _, t := range tokens {
		lt := strings.ToLower(t)
		if len(lt) < 2 || seen[lt] {
			continue
		}
		seen[lt] = true
		terms = append(terms, lt)
	}
	counts := make(map[string]int64, len(terms))
	if len(terms) == 0 {
		return counts, nil
	}

	var sb strings.Builder
	sb.WriteString("SELECT term, doc FROM geocoder_places_fts_vocab WHERE term IN (")
	params := make(dbx.Params, len(terms))
	for i, t := range terms {
		if i > 0 {
			sb.WriteByte(',')
		}
		key := fmt.Sprintf("t%d", i)
		sb.WriteString("{:" + key + "}")
		params[key] = t
	}
	sb.WriteByte(')')

	var rows []struct {
		Term string `db:"term"`
		Doc  int64  `db:"doc"`
	}
	if err := db.NewQuery(sb.String()).Bind(params).All(&rows); err != nil {
		return nil, fmt.Errorf("term doc count lookup failed: %w", err)
	}
	for _, r := range rows {
		counts[r.Term] = r.Doc
	}
	return counts, nil
}

// cachedTermDocCounts returns per-term document counts like termDocCounts,
// but memoized in the Geocoder instance. Query terms are highly repetitive
// ("street", city names), so after warmup the vocab lookup almost never runs.
func (g *Geocoder) cachedTermDocCounts(ctx context.Context, tokens []string) (map[string]int64, error) {
	counts := make(map[string]int64, len(tokens))
	var misses []string

	g.termCountsMu.RLock()
	seen := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		lt := strings.ToLower(t)
		if seen[lt] {
			continue
		}
		seen[lt] = true
		if c, ok := g.termCounts[lt]; ok {
			counts[lt] = c
		} else {
			misses = append(misses, t)
		}
	}
	g.termCountsMu.RUnlock()

	if len(misses) > 0 {
		fresh, err := g.termDocCounts(ctx, misses)
		if err != nil {
			return nil, err
		}
		g.termCountsMu.Lock()
		if len(g.termCounts) > termCountsCacheCap {
			g.termCounts = make(map[string]int64, termCountsCacheCap)
		}
		for _, t := range misses {
			lt := strings.ToLower(t)
			c := fresh[lt] // 0 when absent from the index
			g.termCounts[lt] = c
			counts[lt] = c
		}
		g.termCountsMu.Unlock()
	}
	return counts, nil
}

// minEffectiveCount returns the smallest estimated document count among the
// tokens — the quantity that drives FTS5 intersection cost (other doclists
// are probed from the smallest one). The final token in prefix mode is
// weighted by prefixCostMultiplier. Terms missing from the index count as 0,
// making strict matching trivially cheap. Pure function.
func minEffectiveCount(tokens []string, counts map[string]int64, prefixLast bool) int64 {
	if len(tokens) == 0 {
		return 0
	}
	minC := int64(math.MaxInt64)
	for i, t := range tokens {
		c := counts[strings.ToLower(t)]
		if prefixLast && i == len(tokens)-1 && len(t) >= 3 && len(t) <= 5 {
			c *= prefixCostMultiplier
		}
		if c < minC {
			minC = c
		}
	}
	return minC
}

// pickAnchorTokens selects the tokens that generate fallback candidates. The
// two rarest tokens always participate; further tokens join only while the
// cumulative document count stays tiny (every token rare). Returns the anchor
// tokens and whether they must be AND-ed (both rarest tokens are common, so
// only their intersection is cheap and meaningful). Pure function.
func pickAnchorTokens(tokens []string, counts map[string]int64) (anchors []string, useAND bool) {
	type tokenCount struct {
		t string
		c int64
	}
	seen := make(map[string]bool, len(tokens))
	cands := make([]tokenCount, 0, len(tokens))
	for _, t := range tokens {
		lt := strings.ToLower(t)
		if len(lt) < 3 || seen[lt] { // mirror buildOrQuery's minimum length
			continue
		}
		seen[lt] = true
		cands = append(cands, tokenCount{t, counts[lt]})
	}
	if len(cands) == 0 {
		return nil, false
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].c < cands[j].c })

	anchors = []string{cands[0].t}
	total := cands[0].c
	if len(cands) == 1 {
		return anchors, false
	}
	anchors = append(anchors, cands[1].t)
	total += cands[1].c
	if total > anchorDocThreshold {
		return anchors, true
	}
	for i := 2; i < len(cands) && len(anchors) < 4; i++ {
		if total+cands[i].c > anchorExtendBudget {
			break
		}
		anchors = append(anchors, cands[i].t)
		total += cands[i].c
	}
	return anchors, false
}

// ftsTokens splits a user query into tokens for FTS query building and result
// filtering. Double quotes are stripped since they would break FTS5 quoted
// phrases.
func ftsTokens(q string) []string {
	raw := strings.Fields(q)
	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(strings.ReplaceAll(t, "\"", ""))
		if t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// buildAndQuery builds an FTS5 query requiring every token to match, in any
// field and any order. Tokens shorter than 2 characters are skipped.
func buildAndQuery(tokens []string) string {
	var parts []string
	for _, t := range tokens {
		if len(t) < 2 {
			continue
		}
		parts = append(parts, "\""+t+"\"")
	}
	return strings.Join(parts, " AND ")
}

// buildOrQuery builds a loose FTS5 OR query for ranked fallback matching.
// It mirrors buildPrefixQuery's rules: tokens shorter than 3 characters are
// skipped entirely, and when prefixLast is set the final token (3-5 chars)
// becomes a prefix wildcard.
func buildOrQuery(tokens []string, prefixLast bool) string {
	var parts []string
	for i, t := range tokens {
		if len(t) < 3 {
			continue
		}
		if prefixLast && i == len(tokens)-1 && len(t) <= 5 {
			parts = append(parts, t+"*")
		} else {
			parts = append(parts, "\""+t+"\"")
		}
	}
	return strings.Join(parts, " OR ")
}

// preferMultiTokenMatches reorders OR-fallback candidates so places matching
// at least two query tokens come first (preserving their FTS rank order),
// then fills any remaining slots with single-token matches. Pure function.
func preferMultiTokenMatches(candidates []models.Place, tokens []string, limit int) []models.Place {
	if len(candidates) == 0 {
		return candidates
	}

	lower := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t = strings.ToLower(t); len(t) >= 2 {
			lower = append(lower, t)
		}
	}
	minHits := 2
	if len(lower) < 2 {
		minHits = len(lower)
	}

	multi := make([]models.Place, 0, len(candidates))
	single := make([]models.Place, 0, len(candidates))
	for _, p := range candidates {
		if tokenHits(&p, lower) >= minHits {
			multi = append(multi, p)
		} else {
			single = append(single, p)
		}
	}
	out := append(multi, single...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// tokenHits counts how many of the given tokens appear in the place's
// searchable text fields. Matching is case-insensitive and substring-based so
// autocomplete prefix tokens also count as hits.
func tokenHits(p *models.Place, tokens []string) int {
	haystack := strings.ToLower(p.Name + " " + p.Address + " " + p.City + " " + p.State + " " + p.Postcode)
	hits := 0
	for _, t := range tokens {
		if strings.Contains(haystack, strings.ToLower(t)) {
			hits++
		}
	}
	return hits
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

	g.enrichFromZipCache(ctx, places)
	return places, nil
}

// enrichFromZipCache fills empty City/State fields on search results from the
// zip_city_state cache (derived from OSM data). OSM addresses frequently lack
// addr:city/addr:state tags even when addr:postcode is present. Existing
// values are never overwritten. Uses a single batched lookup (no N+1).
func (g *Geocoder) enrichFromZipCache(ctx context.Context, places []models.Place) {
	seen := make(map[string]bool)
	var zips []string
	for i := range places {
		p := &places[i]
		if (p.City == "" || p.State == "") && p.Postcode != "" && !seen[p.Postcode] {
			seen[p.Postcode] = true
			zips = append(zips, p.Postcode)
		}
	}
	if len(zips) == 0 {
		return
	}

	db := g.app.DB()
	if db == nil {
		return
	}

	var sb strings.Builder
	sb.WriteString("SELECT postcode, COALESCE(city, '') AS city, COALESCE(state, '') AS state FROM zip_city_state WHERE postcode IN (")
	params := dbx.Params{}
	for i, z := range zips {
		if i > 0 {
			sb.WriteByte(',')
		}
		key := fmt.Sprintf("z%d", i)
		sb.WriteString("{:" + key + "}")
		params[key] = z
	}
	sb.WriteByte(')')

	var rows []struct {
		Postcode string `db:"postcode"`
		City     string `db:"city"`
		State    string `db:"state"`
	}
	if err := db.NewQuery(sb.String()).Bind(params).All(&rows); err != nil {
		g.app.Logger().Warn("zip cache enrichment failed", "error", err)
		return
	}

	type cityState struct{ city, state string }
	byZip := make(map[string]cityState, len(rows))
	for _, r := range rows {
		byZip[r.Postcode] = cityState{city: r.City, state: r.State}
	}

	for i := range places {
		p := &places[i]
		cs, ok := byZip[p.Postcode]
		if !ok {
			continue
		}
		if p.City == "" {
			p.City = cs.city
		}
		if p.State == "" {
			p.State = cs.state
		}
	}
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

			rows := make([][]any, len(batch))
			for i, place := range batch {
				rows[i] = []any{
					place.ID, place.OSMID, place.OSMType, place.Name, place.Address,
					place.City, place.State, place.Postcode, place.Country,
					place.Lat, place.Lon, place.Class, place.Type, place.Importance,
				}
			}

			return execMultiValueInsert(txDB,
				`INSERT INTO geocoder_places (
					id, osm_id, osm_type, name, address, city, state, postcode, country,
					lat, lon, class, type, importance, created, updated
				) VALUES `,
				`, datetime('now'), datetime('now')`,
				` ON CONFLICT(id) DO UPDATE SET
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
					updated = datetime('now')`,
				rows)
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

// RebuildFTS rebuilds the places FTS5 index from geocoder_places.
// It runs as a chunked, crash-resumable rebuild (see rebuildFTSIncremental)
// instead of FTS5's single-transaction 'rebuild' command, which is what made
// imports fail on low-memory servers: the WAL could not be checkpointed for
// the duration of a ~54M-row transaction and the whole rebuild rolled back
// if the process was killed.
func (g *Geocoder) RebuildFTS(ctx context.Context) error {
	return g.rebuildFTSIncremental(ctx, "geocoder_places_fts", "geocoder_places",
		[]string{"name", "address", "city", "state", "postcode"},
		StateOSMFTSOffset, StateOSMFTSDone)
}

// rebuildFTSIncremental repopulates an FTS5 external-content index in
// committed rowid-ordered chunks, persisting progress in _import_state after
// every chunk so an interrupted rebuild resumes where it left off.
//
// Table and column names are internal constants, never user input, so direct
// SQL interpolation is safe here.
func (g *Geocoder) rebuildFTSIncremental(ctx context.Context, ftsTable, contentTable string, columns []string, offsetKey, doneKey string) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	chunkSize := defaultFTSRebuildChunkSize
	if g.cfg != nil && g.cfg.FTSRebuildChunkSize > 0 {
		chunkSize = g.cfg.FTSRebuildChunkSize
	}

	colList := strings.Join(columns, ", ")

	// Resume from the last committed chunk if a previous rebuild was interrupted.
	var after int64
	switch v, err := g.GetImportState(ctx, offsetKey); {
	case err != nil:
		return fmt.Errorf("failed to read %s rebuild progress: %w", ftsTable, err)
	case v != "":
		after, _ = strconv.ParseInt(v, 10, 64)
		log.Printf("%s: resuming interrupted rebuild from rowid %d", ftsTable, after)
	default:
		// Fresh rebuild: wipe existing FTS entries (the content table is untouched).
		if _, err := db.NewQuery(fmt.Sprintf("INSERT INTO %s(%s) VALUES('delete-all')", ftsTable, ftsTable)).Execute(); err != nil {
			return fmt.Errorf("failed to clear %s: %w", ftsTable, err)
		}
	}

	// Upper bound, used only for progress reporting.
	var maxRowID int64
	_ = db.NewQuery(fmt.Sprintf("SELECT COALESCE(MAX(rowid), 0) FROM %s", contentTable)).Row(&maxRowID)

	boundQuery := fmt.Sprintf(
		"SELECT COALESCE(MAX(rowid), 0), COUNT(*) FROM (SELECT rowid FROM %s WHERE rowid > {:after} ORDER BY rowid LIMIT {:n})",
		contentTable)
	insertQuery := fmt.Sprintf(
		"INSERT INTO %s(rowid, %s) SELECT rowid, %s FROM %s WHERE rowid > {:after} AND rowid <= {:upto}",
		ftsTable, colList, colList, contentTable)

	startTime := time.Now()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Find the rowid bounding the next chunk (memory stays bounded
		// regardless of table size).
		var upto, count int64
		if err := db.NewQuery(boundQuery).Bind(dbx.Params{"after": after, "n": chunkSize}).Row(&upto, &count); err != nil {
			return fmt.Errorf("failed to scan %s for rebuild: %w", contentTable, err)
		}
		if count == 0 {
			break
		}

		txErr := g.app.RunInTransaction(func(txApp core.App) error {
			txDB := txApp.NonconcurrentDB()
			if txDB == nil {
				return fmt.Errorf("transaction db is not available")
			}
			if _, err := txDB.NewQuery(insertQuery).Bind(dbx.Params{"after": after, "upto": upto}).Execute(); err != nil {
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
			return fmt.Errorf("%s rebuild failed at rowid %d: %w", ftsTable, upto, txErr)
		}

		after = upto
		if maxRowID > 0 {
			log.Printf("%s rebuild progress: rowid %d/%d (%.0f%%)", ftsTable, after, maxRowID, float64(after)/float64(maxRowID)*100)
		} else {
			log.Printf("%s rebuild progress: rowid %d", ftsTable, after)
		}
	}

	if err := g.DeleteImportState(ctx, offsetKey); err != nil {
		log.Printf("warning: failed to clear %s rebuild progress: %v", ftsTable, err)
	}
	if err := g.SetImportState(ctx, doneKey, "done"); err != nil {
		return fmt.Errorf("failed to mark %s rebuild complete: %w", ftsTable, err)
	}

	log.Printf("%s rebuild complete in %s", ftsTable, time.Since(startTime).Round(time.Second))
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
//
// Two changes keep this safe on low-memory servers:
//   - The new table is built alongside the old one and swapped in atomically,
//     so a crash mid-rebuild can't leave the table missing (which would break
//     TIGER interpolation queries at runtime).
//   - The "most common city/state per ZIP" uses SQLite's bare-column-with-MAX
//     idiom instead of a window function, roughly halving the sort work over
//     the tens-of-millions-row places table.
func (g *Geocoder) RebuildZipCache(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	if _, err := db.NewQuery("DROP TABLE IF EXISTS zip_city_state_new").Execute(); err != nil {
		return fmt.Errorf("failed to drop stale zip cache: %w", err)
	}

	// For each postcode, city/state come from the row with the highest count
	// (documented SQLite behavior for bare columns alongside MAX()).
	if _, err := db.NewQuery(`
		CREATE TABLE zip_city_state_new AS
		SELECT postcode, city, state FROM (
			SELECT postcode, city, state, MAX(cnt) AS cnt FROM (
				SELECT postcode, city, state, COUNT(*) AS cnt
				FROM geocoder_places
				WHERE city != '' AND postcode != ''
				GROUP BY postcode, city, state
			) GROUP BY postcode
		)
	`).Execute(); err != nil {
		return fmt.Errorf("failed to build zip cache: %w", err)
	}

	txErr := g.app.RunInTransaction(func(txApp core.App) error {
		txDB := txApp.NonconcurrentDB()
		if txDB == nil {
			return fmt.Errorf("transaction db is not available")
		}
		if _, err := txDB.NewQuery("DROP TABLE IF EXISTS zip_city_state").Execute(); err != nil {
			return err
		}
		if _, err := txDB.NewQuery("ALTER TABLE zip_city_state_new RENAME TO zip_city_state").Execute(); err != nil {
			return err
		}
		if _, err := txDB.NewQuery("CREATE INDEX IF NOT EXISTS idx_zip_city_state ON zip_city_state(postcode)").Execute(); err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return fmt.Errorf("failed to swap zip cache table: %w", txErr)
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

// BatchInsertNodeCoords inserts node coordinates in batches using multi-row
// INSERT statements (see execMultiValueInsert). Re-runs are idempotent.
func (g *Geocoder) BatchInsertNodeCoords(ctx context.Context, coords []NodeCoord) error {
	if len(coords) == 0 {
		return nil
	}

	txErr := g.app.RunInTransaction(func(txApp core.App) error {
		txDB := txApp.NonconcurrentDB()
		if txDB == nil {
			return fmt.Errorf("transaction db is not available")
		}

		rows := make([][]any, len(coords))
		for i, c := range coords {
			rows[i] = []any{c.OSMID, c.Lat, c.Lon}
		}
		return execMultiValueInsert(txDB,
			"INSERT OR REPLACE INTO _osm_node_coords (osm_id, lat, lon) VALUES ", "", "", rows)
	})

	if txErr != nil {
		return fmt.Errorf("batch insert node coords failed: %w", txErr)
	}

	return nil
}

// LookupNodeCoords returns the stored coordinates for the given node IDs.
// IDs absent from _osm_node_coords (e.g. clipped at extract boundaries) are
// simply missing from the result map. Lookup keys are chunked to stay under
// SQLite's bound-variable limit.
func (g *Geocoder) LookupNodeCoords(ctx context.Context, ids []int64) (map[int64][2]float64, error) {
	coords := make(map[int64][2]float64, len(ids))
	if len(ids) == 0 {
		return coords, nil
	}

	db := g.app.DB()
	if db == nil {
		return nil, fmt.Errorf("db is not available")
	}

	for start := 0; start < len(ids); start += maxInsertVars {
		end := start + maxInsertVars
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		var sb strings.Builder
		sb.WriteString("SELECT osm_id, lat, lon FROM _osm_node_coords WHERE osm_id IN (")
		params := make(dbx.Params, len(chunk))
		for i, id := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			key := fmt.Sprintf("n%d", i)
			sb.WriteString("{:" + key + "}")
			params[key] = id
		}
		sb.WriteByte(')')

		var rows []struct {
			OSMID int64   `db:"osm_id"`
			Lat   float64 `db:"lat"`
			Lon   float64 `db:"lon"`
		}
		if err := db.NewQuery(sb.String()).Bind(params).All(&rows); err != nil {
			return nil, fmt.Errorf("lookup node coords failed: %w", err)
		}
		for _, r := range rows {
			coords[r.OSMID] = [2]float64{r.Lat, r.Lon}
		}
	}

	return coords, nil
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

// WayCoordUpdate assigns a resolved centroid to one way place row.
type WayCoordUpdate struct {
	ID  string // place ID, e.g. "way/12345"
	Lat float64
	Lon float64
}

// CreateWayCoordUpdateTable creates the scratch table used to batch way
// centroid updates. It lives in the main database (not TEMP) so the batched
// UPDATE ... FROM can join against geocoder_places, and it is dropped when
// the resolution phase completes.
func (g *Geocoder) CreateWayCoordUpdateTable(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	if _, err := db.NewQuery(`CREATE TABLE IF NOT EXISTS _way_coord_updates (
id TEXT PRIMARY KEY,
lat REAL NOT NULL,
lon REAL NOT NULL
)`).Execute(); err != nil {
		return fmt.Errorf("failed to create way coord update table: %w", err)
	}
	// Clear leftovers from an interrupted earlier run.
	if _, err := db.NewQuery("DELETE FROM _way_coord_updates").Execute(); err != nil {
		return fmt.Errorf("failed to clear way coord update table: %w", err)
	}
	return nil
}

// DropWayCoordUpdateTable drops the centroid-update scratch table.
func (g *Geocoder) DropWayCoordUpdateTable(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}
	if _, err := db.NewQuery("DROP TABLE IF EXISTS _way_coord_updates").Execute(); err != nil {
		return fmt.Errorf("failed to drop way coord update table: %w", err)
	}
	return nil
}

// ApplyWayCoordUpdates applies one chunk of way centroid assignments and
// records the scan position (afterWayID) in the same transaction, so an
// interrupted resolution pass resumes exactly after the last committed way.
func (g *Geocoder) ApplyWayCoordUpdates(ctx context.Context, updates []WayCoordUpdate, afterWayID int64) error {
	txErr := g.app.RunInTransaction(func(txApp core.App) error {
		txDB := txApp.NonconcurrentDB()
		if txDB == nil {
			return fmt.Errorf("transaction db is not available")
		}

		if len(updates) > 0 {
			rows := make([][]any, len(updates))
			for i, u := range updates {
				rows[i] = []any{u.ID, u.Lat, u.Lon}
			}
			if err := execMultiValueInsert(txDB,
				"INSERT OR REPLACE INTO _way_coord_updates (id, lat, lon) VALUES ", "", "", rows); err != nil {
				return err
			}
			if _, err := txDB.NewQuery(`UPDATE geocoder_places SET lat = u.lat, lon = u.lon
				FROM _way_coord_updates u WHERE geocoder_places.id = u.id`).Execute(); err != nil {
				return fmt.Errorf("failed to apply way coord updates: %w", err)
			}
			if _, err := txDB.NewQuery("DELETE FROM _way_coord_updates").Execute(); err != nil {
				return err
			}
		}

		_, err := txDB.NewQuery(`INSERT INTO _import_state (key, value) VALUES ({:key}, {:value})
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`).
			Bind(dbx.Params{"key": StateOSMWayCoordsOffset, "value": strconv.FormatInt(afterWayID, 10)}).Execute()
		return err
	})

	if txErr != nil {
		return fmt.Errorf("apply way coord updates failed: %w", txErr)
	}
	return nil
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

// GetImportState retrieves a value from the _import_state table.
// Returns "" if the key doesn't exist (table may not exist yet on first run).
func (g *Geocoder) GetImportState(ctx context.Context, key string) (string, error) {
	db := g.app.DB()
	if db == nil {
		return "", fmt.Errorf("db is not available")
	}

	var value string
	err := db.NewQuery("SELECT value FROM _import_state WHERE key = {:key}").
		Bind(dbx.Params{"key": key}).Row(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetImportState sets a key-value pair in the _import_state table.
func (g *Geocoder) SetImportState(ctx context.Context, key, value string) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	_, err := db.NewQuery(`INSERT INTO _import_state (key, value) VALUES ({:key}, {:value})
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`).
		Bind(dbx.Params{"key": key, "value": value}).Execute()
	if err != nil {
		return fmt.Errorf("failed to set import state %s: %w", key, err)
	}
	return nil
}

// DeleteImportState removes a key from the _import_state table.
func (g *Geocoder) DeleteImportState(ctx context.Context, key string) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	if _, err := db.NewQuery("DELETE FROM _import_state WHERE key = {:key}").
		Bind(dbx.Params{"key": key}).Execute(); err != nil {
		return fmt.Errorf("failed to delete import state %s: %w", key, err)
	}
	return nil
}

// Checkpoint flushes the WAL back into the main database file (truncating the
// WAL) and refreshes query planner statistics. Call after large bulk-write
// phases to reclaim disk space on small servers. Errors are logged but not
// treated as fatal.
func (g *Geocoder) Checkpoint(ctx context.Context) {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return
	}

	var busy, logFrames, checkpointed int
	if err := db.NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Row(&busy, &logFrames, &checkpointed); err != nil {
		log.Printf("warning: wal_checkpoint failed: %v", err)
	}
	if _, err := db.NewQuery("PRAGMA optimize").Execute(); err != nil {
		log.Printf("warning: PRAGMA optimize failed: %v", err)
	}
}

// IsTigerCountyImported returns true if the given county FIPS code has already
// been imported. This enables resuming an interrupted TIGER import without
// re-processing completed counties.
func (g *Geocoder) IsTigerCountyImported(ctx context.Context, fips string) (bool, error) {
	v, err := g.GetImportState(ctx, "tiger_county_"+fips)
	if err != nil {
		return false, err
	}
	// Accept legacy "done" markers as well as "done:<rows>" ones.
	return v == "done" || strings.HasPrefix(v, "done:"), nil
}

// MarkTigerCountyImported marks a county as imported in the _import_state
// table, recording how many address ranges were imported. The count makes
// silently-empty imports visible (done:0) in the startup marker review and
// in SQL reviews.
func (g *Geocoder) MarkTigerCountyImported(ctx context.Context, fips string, rows int) error {
	return g.SetImportState(ctx, "tiger_county_"+fips, fmt.Sprintf("done:%d", rows))
}

// CleanupOldTigerTable removes the pre-migration tiger_addr_ranges_old table
// left behind by the one-time conflict-key migration. The old table's rows are
// deleted in committed chunks — dropping a ~33M-row table outright writes
// every freed page to the WAL freelist in one transaction, which is fatal on
// small servers. The final DROP of the empty table is metadata-only. Safe to
// call on every startup: it is a no-op when the table is absent, and it
// resumes wherever it stopped if interrupted.
func (g *Geocoder) CleanupOldTigerTable(ctx context.Context) error {
	db := g.app.DB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	var exists int
	_ = db.NewQuery("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='tiger_addr_ranges_old'").Row(&exists)
	if exists == 0 {
		return nil
	}

	log.Printf("cleaning up pre-migration tiger_addr_ranges_old table (chunked delete)...")
	const chunkSize = 250000
	startTime := time.Now()
	deleted := int64(0)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var n int64
		txErr := g.app.RunInTransaction(func(txApp core.App) error {
			txDB := txApp.NonconcurrentDB()
			if txDB == nil {
				return fmt.Errorf("transaction db is not available")
			}
			res, err := txDB.NewQuery(
				"DELETE FROM tiger_addr_ranges_old WHERE rowid IN (SELECT rowid FROM tiger_addr_ranges_old LIMIT {:n})",
			).Bind(dbx.Params{"n": chunkSize}).Execute()
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
			return nil
		})
		if txErr != nil {
			return fmt.Errorf("failed to delete old tiger rows: %w", txErr)
		}
		if n == 0 {
			break
		}
		deleted += n
		log.Printf("old tiger table cleanup progress: deleted %d rows", deleted)
	}

	if _, err := db.NewQuery("DROP TABLE tiger_addr_ranges_old").Execute(); err != nil {
		return fmt.Errorf("failed to drop tiger_addr_ranges_old: %w", err)
	}

	log.Printf("old tiger table cleanup complete: deleted %d rows in %s", deleted, time.Since(startTime).Round(time.Second))
	return nil
}

// ReviewTigerMarkers returns counties whose TIGER import markers look
// suspicious: legacy "done" markers without a recorded row count (their
// completeness can't be verified), and "done:0" markers (the import completed
// but recorded no address ranges — worth re-checking if addresses are missing
// in that county).
func (g *Geocoder) ReviewTigerMarkers(ctx context.Context) (legacy, zero []string, err error) {
	db := g.app.DB()
	if db == nil {
		return nil, nil, fmt.Errorf("db is not available")
	}

	var rows []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	if err := db.NewQuery("SELECT key, value FROM _import_state WHERE key LIKE 'tiger_county_%'").All(&rows); err != nil {
		return nil, nil, fmt.Errorf("failed to read tiger import markers: %w", err)
	}

	for _, r := range rows {
		switch r.Value {
		case "done":
			legacy = append(legacy, strings.TrimPrefix(r.Key, "tiger_county_"))
		case "done:0":
			zero = append(zero, strings.TrimPrefix(r.Key, "tiger_county_"))
		}
	}
	return legacy, zero, nil
}

// ClearTigerImportState removes all TIGER progress markers (per-county markers
// and FTS rebuild state). Called when a full TIGER re-import is forced.
func (g *Geocoder) ClearTigerImportState(ctx context.Context) error {
	db := g.app.NonconcurrentDB()
	if db == nil {
		return fmt.Errorf("db is not available")
	}

	_, err := db.NewQuery("DELETE FROM _import_state WHERE key LIKE 'tiger_%'").Execute()
	if err != nil {
		return fmt.Errorf("failed to clear tiger import state: %w", err)
	}
	return nil
}

// RebuildTigerFTS rebuilds the TIGER FTS5 index from the tiger_addr_ranges table.
// Like RebuildFTS, it runs as a chunked, crash-resumable rebuild to keep WAL
// growth and memory bounded on low-memory servers.
func (g *Geocoder) RebuildTigerFTS(ctx context.Context) error {
	return g.rebuildFTSIncremental(ctx, "tiger_addr_fts", "tiger_addr_ranges",
		[]string{"full_name"}, StateTigerFTSOffset, StateTigerFTSDone)
}

// normalizeStreetName expands common abbreviations so that user queries like
// "Maple Street" match TIGER data which uses "Maple St".
func normalizeStreetName(name string) string {
	name = strings.TrimSpace(name)
	// Replace whole-word full spellings with TIGER abbreviations (case-insensitive).
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
		{"Terrace", "Ter"},
		{"Trail", "Trl"},
		{"Parkway", "Pkwy"},
		{"Highway", "Hwy"},
		{"Square", "Sq"},
		{"Crossing", "Xing"},
		{"Turnpike", "Tpke"},
		{"Alley", "Aly"},
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

// exactMatchCap bounds how many nationwide TIGER candidates are fetched for a
// single street+number lookup before ranking (e.g. "Main St" matches tens of
// thousands of ranges). The best `limit` rows are chosen after ranking, so a
// generous cap is sufficient.
const exactMatchCap = 500

// usStreetSuffixes are common USPS street suffix tokens (abbreviated and full
// spellings). The first one in a query marks the end of the street name;
// anything after it is treated as city/state context.
//
// Note: suffix words that also commonly appear INSIDE street names (e.g.
// "park", "ridge", "creek", "lake", "hill", "valley", "mount") are
// deliberately excluded — adding them would split "Birchwood Park Dr" or
// "Birchwood Ridge Pl" at the wrong place.
var usStreetSuffixes = map[string]bool{
	"st": true, "street": true,
	"ave": true, "avenue": true,
	"rd": true, "road": true,
	"dr": true, "drive": true,
	"ln": true, "lane": true,
	"ct": true, "court": true,
	"blvd": true, "boulevard": true,
	"pl": true, "place": true,
	"cir": true, "circle": true,
	"ter": true, "terrace": true,
	"trl": true, "trail": true,
	"pkwy": true, "parkway": true,
	"hwy": true, "highway": true,
	"sq": true, "square": true,
	"xing": true, "crossing": true,
	"way": true, "loop": true,
	"pass": true, "path": true, "row": true, "run": true, "walk": true,
	"aly": true, "alley": true,
	"pike": true, "tpke": true, "turnpike": true,
}

// usStateAbbrevs is the set of valid US state/territory abbreviations, used to
// detect a trailing state token in a query (e.g. "... capitan nm").
var usStateAbbrevs = map[string]bool{
	"AL": true, "AK": true, "AZ": true, "AR": true, "CA": true,
	"CO": true, "CT": true, "DE": true, "DC": true, "FL": true,
	"GA": true, "HI": true, "ID": true, "IL": true, "IN": true,
	"IA": true, "KS": true, "KY": true, "LA": true, "ME": true,
	"MD": true, "MA": true, "MI": true, "MN": true, "MS": true,
	"MO": true, "MT": true, "NE": true, "NV": true, "NH": true,
	"NJ": true, "NM": true, "NY": true, "NC": true, "ND": true,
	"OH": true, "OK": true, "OR": true, "PA": true, "RI": true,
	"SC": true, "SD": true, "TN": true, "TX": true, "UT": true,
	"VT": true, "VA": true, "WA": true, "WV": true, "WI": true,
	"WY": true, "PR": true,
}

// likeWildcardReplacer removes LIKE wildcard characters from user input so the
// city context filter treats them literally.
var likeWildcardReplacer = strings.NewReplacer("%", "", "_", "")

// splitStreetContext splits a street query like "birchwood rd capitan nm"
// into the street part ("birchwood rd") and trailing city/state context
// ("capitan nm") at the first recognized street suffix. Returns the full
// string as the street and an empty context when no suffix boundary is found.
func splitStreetContext(s string) (street, context string) {
	tokens := strings.Fields(s)
	// Start at index 1: a suffix as the very first token can't end the street
	// name (e.g. "St Marys Rd" where "St" means "Saint").
	for i := 1; i < len(tokens); i++ {
		if usStreetSuffixes[strings.ToLower(strings.TrimRight(tokens[i], "."))] {
			if i+1 < len(tokens) {
				return strings.Join(tokens[:i+1], " "), strings.Join(tokens[i+1:], " ")
			}
			return s, ""
		}
	}
	return s, ""
}

// splitContextState extracts a trailing US state abbreviation from a query's
// city/state context, e.g. "capitan nm" -> ("capitan", "NM").
// Returns the full context as city when no state token is found.
func splitContextState(context string) (city, state string) {
	tokens := strings.Fields(context)
	if len(tokens) == 0 {
		return "", ""
	}
	last := strings.ToUpper(strings.TrimRight(tokens[len(tokens)-1], "."))
	if usStateAbbrevs[last] {
		return strings.Join(tokens[:len(tokens)-1], " "), last
	}
	return context, ""
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
//
// Candidate ranking: when a bbox is provided, candidates are filtered to it
// and ordered by proximity to its center (squared distance, longitude scaled
// by cos(center latitude)); otherwise they are ordered by interpolation
// precision (narrowest address range first). Precision ordering is
// deterministic across databases, unlike rowid order, which depends on
// import order and made the top-N cut arbitrary for streets with many
// nationwide candidates.
//
// City/state context: trailing tokens after the street suffix are treated as
// city/state context (e.g. "birchwood rd capitan nm"), and matching
// candidates are ranked ahead of unfiltered nationwide ones.
func (g *Geocoder) InterpolateAddress(ctx context.Context, houseNumber int, streetName string, limit int, bbox *BBox) ([]models.Place, error) {
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
				Name:     fmt.Sprintf("%d %s", houseNumber, ar.FullName),
				Address:  fmt.Sprintf("%d %s", houseNumber, ar.FullName),
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
	// Title-case the street name to match TIGER's naming convention (e.g., "Birchwood Rd").
	// This enables case-sensitive index lookups which are much faster than COLLATE NOCASE.
	titleCased := titleCase(streetName)
	normalized := titleCase(normalizeStreetName(streetName))

	// Build the shared bbox filter and ranking clause (see the function doc).
	bboxWhere := ""
	orderBy := "ORDER BY ABS(t.to_hn - t.from_hn), t.rowid"
	sharedParams := dbx.Params{}
	if bbox != nil && bbox.Valid() {
		cLat := (bbox.MinLat + bbox.MaxLat) / 2
		cLon := (bbox.MinLng + bbox.MaxLng) / 2
		lonScale := math.Cos(cLat * math.Pi / 180)
		bboxWhere = `AND t.lat >= {:min_lat} AND t.lat <= {:max_lat}
		  AND t.lon >= {:min_lng} AND t.lon <= {:max_lng}`
		orderBy = `ORDER BY (t.lat - {:c_lat}) * (t.lat - {:c_lat}) +
		                    (t.lon - {:c_lon}) * (t.lon - {:c_lon}) * {:lon_scale} * {:lon_scale},
		                    ABS(t.to_hn - t.from_hn), t.rowid`
		sharedParams["min_lat"] = bbox.MinLat
		sharedParams["max_lat"] = bbox.MaxLat
		sharedParams["min_lng"] = bbox.MinLng
		sharedParams["max_lng"] = bbox.MaxLng
		sharedParams["c_lat"] = cLat
		sharedParams["c_lon"] = cLon
		sharedParams["lon_scale"] = lonScale
	}

	// Split trailing city/state context off the street name, e.g.
	// "birchwood rd capitan nm" -> street "birchwood rd", city
	// "capitan", state "NM". City/state-filtered matches rank first;
	// unfiltered nationwide candidates are appended after them.
	streetPart, contextPart := splitStreetContext(streetName)
	cityCtx, stateCtx := splitContextState(contextPart)
	cityWhere := ""
	cityParams := dbx.Params{}
	if cityCtx != "" {
		cityWhere = " AND z.city LIKE {:city_ctx}"
		cityParams["city_ctx"] = likeWildcardReplacer.Replace(cityCtx) + "%"
	}
	if stateCtx != "" {
		cityWhere += " AND z.state = {:state_ctx}"
		cityParams["state_ctx"] = stateCtx
	}

	// Note: COALESCE is required because zip_city_state doesn't cover every ZIP;
	// without it, scanning NULL city/state into strings fails and the whole
	// interpolation silently returns no results.
	// runExact executes one exact-name lookup pass with an optional extra
	// WHERE fragment (the city/state filter).
	runExact := func(street1, street2, extraWhere string, extraParams dbx.Params) error {
		exactQuery := fmt.Sprintf(`
			SELECT t.full_name, t.from_hn, t.to_hn, t.parity, t.zip, t.side, t.lat, t.lon,
			       COALESCE(z.city, '') AS city, COALESCE(z.state, '') AS state
			FROM tiger_addr_ranges t
			LEFT JOIN zip_city_state z ON z.postcode = t.zip
			WHERE t.full_name IN ({:street1}, {:street2})
			  AND ((t.from_hn <= {:hn} AND t.to_hn >= {:hn}) OR (t.to_hn <= {:hn} AND t.from_hn >= {:hn}))
			  AND (t.parity = {:parity} OR t.parity = 'B')
			  %s
			  %s
			%s
			LIMIT {:cap}
		`, bboxWhere, extraWhere, orderBy)
		exactParams := dbx.Params{
			"street1": street1,
			"street2": street2,
			"hn":      houseNumber,
			"parity":  parity,
			"cap":     exactMatchCap,
		}
		for k, v := range sharedParams {
			exactParams[k] = v
		}
		for k, v := range extraParams {
			exactParams[k] = v
		}
		var exactRows []tigerAddrRow
		if err := db.NewQuery(exactQuery).Bind(exactParams).All(&exactRows); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("tiger address lookup failed: %w", err)
		}
		buildPlaces(exactRows)
		return nil
	}

	enough := func() bool { return len(results) >= limit }

	// 1a. Exact match on the street part with the city/state context filter
	// (most specific intent, e.g. "Birchwood Rd" in Capitan).
	if cityWhere != "" {
		if err := runExact(titleCase(streetPart), titleCase(normalizeStreetName(streetPart)), cityWhere, cityParams); err != nil {
			return nil, err
		}
		if enough() {
			return results[:limit], nil
		}
	}

	// 1b. Exact match on the full street string (covers street names that
	// legitimately contain the context word, and is the no-context path).
	if err := runExact(titleCased, normalized, "", nil); err != nil {
		return nil, err
	}
	if enough() {
		return results[:limit], nil
	}

	// 1c. Exact match on the street part without the city filter, so
	// nationwide candidates still appear after city-filtered ones.
	if streetPart != streetName {
		if err := runExact(titleCase(streetPart), titleCase(normalizeStreetName(streetPart)), "", nil); err != nil {
			return nil, err
		}
		if enough() {
			return results[:limit], nil
		}
	}

	// 2. Fall back to FTS prefix match on the first word of the street part.
	// This uses the tiger_addr_fts index instead of a LIKE full table scan.
	// e.g., "main*" matches "Main St", "Main Rd", "Maine St", etc.
	if parts := strings.Fields(streetPart); len(parts) > 0 {
		remaining := limit - len(results)
		ftsQuery := parts[0] + "*"
		ftsQuerySQL := fmt.Sprintf(`
			SELECT t.full_name, t.from_hn, t.to_hn, t.parity, t.zip, t.side, t.lat, t.lon,
			       COALESCE(z.city, '') AS city, COALESCE(z.state, '') AS state
			FROM tiger_addr_ranges t
			INNER JOIN tiger_addr_fts f ON t.rowid = f.rowid
			LEFT JOIN zip_city_state z ON z.postcode = t.zip
			WHERE tiger_addr_fts MATCH {:fts_query}
			  AND ((t.from_hn <= {:hn} AND t.to_hn >= {:hn}) OR (t.to_hn <= {:hn} AND t.from_hn >= {:hn}))
			  AND (t.parity = {:parity} OR t.parity = 'B')
			  %s
			%s
			LIMIT {:limit}
		`, bboxWhere, orderBy)
		ftsParams := dbx.Params{
			"fts_query": ftsQuery,
			"hn":        houseNumber,
			"parity":    parity,
			"limit":     remaining,
		}
		for k, v := range sharedParams {
			ftsParams[k] = v
		}
		var ftsRows []tigerAddrRow
		if err := db.NewQuery(ftsQuerySQL).Bind(ftsParams).All(&ftsRows); err != nil && err != sql.ErrNoRows {
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

			rows := make([][]any, len(batch))
			for i, ar := range batch {
				rows[i] = []any{ar.FullName, ar.FromHN, ar.ToHN, ar.Parity, ar.ZIP, ar.Side, ar.Lat, ar.Lon}
			}

			return execMultiValueInsert(txDB,
				`INSERT INTO tiger_addr_ranges (full_name, from_hn, to_hn, parity, zip, side, lat, lon) VALUES `,
				"",
				` ON CONFLICT(full_name, from_hn, to_hn, side, zip) DO UPDATE SET
					parity = excluded.parity, lat = excluded.lat, lon = excluded.lon`,
				rows)
		})

		if txErr != nil {
			return saved, fmt.Errorf("batch upsert addr ranges failed at offset %d: %w", i, txErr)
		}

		saved += len(batch)
	}

	return saved, nil
}
