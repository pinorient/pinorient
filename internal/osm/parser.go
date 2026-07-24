package osm

import (
	"context"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/qedus/osmpbf"

	"github.com/pinorient/pinorient/internal/geocoder"
	"github.com/pinorient/pinorient/internal/models"
)

// defaultBatchSize is the fallback batch size if none is configured.
const defaultBatchSize = 2000

// progressInterval is how often (in records) to log import progress.
const progressInterval = 50000

// Parser reads OSM PBF data and indexes relevant places.
type Parser struct {
	geo        *geocoder.Geocoder
	batchSize  int
	numWorkers int
}

// NewParser creates a new OSM parser with the given batch size and decoder worker count.
// If batchSize is <= 0, defaults to 2000 (safe for 2GB RAM servers).
// If numWorkers is <= 0, defaults to runtime.NumCPU().
func NewParser(geo *geocoder.Geocoder, batchSize, numWorkers int) *Parser {
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	if numWorkers <= 0 {
		numWorkers = runtime.NumCPU()
	}
	return &Parser{geo: geo, batchSize: batchSize, numWorkers: numWorkers}
}

// Parse reads OSM PBF data from r and indexes addressable places.
// Both nodes and ways are indexed in a single pass. Ways are indexed without
// coordinates (lat/lon = 0) because the PBF format gives ways no node
// geometry; the scheduler resolves their centroids from the same extract in a
// later phase (see waycoords.go).
//
// FTS triggers are temporarily dropped during import to avoid per-row FTS
// overhead. The caller is responsible for rebuilding the FTS index afterwards
// (the scheduler runs a chunked, crash-resumable rebuild via RebuildFTS).
func (p *Parser) Parse(ctx context.Context, r io.Reader) error {
	// Drop FTS triggers to speed up bulk inserts.
	if err := p.geo.DropFTSTriggers(ctx); err != nil {
		return fmt.Errorf("failed to drop FTS triggers: %w", err)
	}
	defer func() {
		if err := p.geo.CreateFTSTriggers(ctx); err != nil {
			log.Printf("warning: failed to recreate FTS triggers: %v", err)
		}
	}()

	decoder := osmpbf.NewDecoder(r)
	if err := decoder.Start(p.numWorkers); err != nil {
		return fmt.Errorf("failed to start osm decoder: %w", err)
	}

	startTime := time.Now()
	totalIndexed := 0
	skipped := 0
	buffer := make([]*models.Place, 0, p.batchSize)

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		saved, err := p.geo.BatchUpsertPlaces(ctx, buffer, p.batchSize)
		if err != nil {
			return err
		}
		totalIndexed += saved
		buffer = buffer[:0]
		return nil
	}

	for {
		if ctx.Err() != nil {
			_ = flush()
			return ctx.Err()
		}

		v, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = flush()
			return fmt.Errorf("decode error: %w", err)
		}

		var place *models.Place
		switch obj := v.(type) {
		case *osmpbf.Node:
			place = nodeToPlace(obj)
		case *osmpbf.Way:

			place = wayToPlace(obj)
		default:
			continue
		}

		if place != nil {
			buffer = append(buffer, place)
			if len(buffer) >= p.batchSize {
				if err := flush(); err != nil {
					return err
				}
				if totalIndexed%progressInterval < p.batchSize {
					log.Printf("osm import progress: indexed=%d skipped=%d elapsed=%s",
						totalIndexed, skipped, time.Since(startTime).Round(time.Second))
				}
			}
		} else {
			skipped++
		}
	}

	if err := flush(); err != nil {
		return err
	}

	elapsed := time.Since(startTime)
	log.Printf("osm row import complete: indexed=%d skipped=%d elapsed=%s (FTS rebuild is handled by the scheduler)",
		totalIndexed, skipped, elapsed.Round(time.Second))

	return nil
}

// hasAddressTags checks if the tags contain address information.
func hasAddressTags(tags map[string]string) bool {
	for k := range tags {
		if strings.HasPrefix(k, "addr:") {
			return true
		}
	}
	return false
}

// isIndexedWay reports whether a way becomes a geocoder_places row. It mirrors
// the acceptance rules in wayToPlace (a name, or enough addr: tags to build
// one) and is used by the way-coordinates phase to select exactly the ways
// that need centroids.
func isIndexedWay(w *osmpbf.Way) bool {
	return w.Tags["name"] != "" || w.Tags["addr:housenumber"] != "" || w.Tags["addr:street"] != ""
}

// buildAddress constructs a display address string from addr: tags.
func buildAddress(tags map[string]string) string {
	var parts []string

	housenumber := firstTag(tags, "addr:housenumber")
	street := firstTag(tags, "addr:street")
	if housenumber != "" && street != "" {
		parts = append(parts, housenumber+" "+street)
	} else if street != "" {
		parts = append(parts, street)
	} else if housenumber != "" {
		parts = append(parts, housenumber)
	}

	return strings.Join(parts, ", ")
}

// classKeys lists OSM tag keys that define a feature's class, in priority
// order (most descriptive first). Photon/Nominatim derive their
// osm_key/osm_value (class/type) the same way; OSM itself has no literal
// "class" tag, so reading tags["class"] always came back empty.
var classKeys = []string{
	"place", "boundary", "amenity", "healthcare", "office", "shop", "craft",
	"tourism", "leisure", "historic", "sport", "public_transport", "railway",
	"aeroway", "highway", "waterway", "natural", "landuse", "building",
	"man_made", "power", "military", "emergency", "telecom",
}

// classType derives the (class, type) pair for a feature from its OSM tags,
// e.g. {amenity: "university"} -> ("amenity", "university"). The first key
// from classKeys present on the feature wins.
func classType(tags map[string]string) (class, typ string) {
	for _, k := range classKeys {
		if v := tags[k]; v != "" {
			return k, v
		}
	}
	return "", ""
}

// placeImportance assigns a coarse importance score (0-1) to a feature for
// result ranking: named settlements outrank named POIs, which outrank street
// addresses. (Stored for ranking/Photon compatibility; OSM provides no
// importance signal of its own.)
func placeImportance(tags map[string]string, class string) float64 {
	switch tags["place"] {
	case "city":
		return 0.45
	case "town":
		return 0.40
	case "village":
		return 0.35
	case "suburb", "quarter", "borough":
		return 0.30
	case "neighbourhood":
		return 0.28
	case "hamlet", "locality":
		return 0.25
	case "island", "islet":
		return 0.20
	}

	if tags["name"] == "" {
		return 0
	}
	switch class {
	case "amenity", "healthcare", "office", "shop", "tourism", "leisure",
		"historic", "aeroway", "railway", "natural", "waterway":
		return 0.15
	case "highway":
		return 0.10
	}
	return 0.05
}

// nodeToPlace converts an OSM node to a Place if it has a name or address tags.
func nodeToPlace(n *osmpbf.Node) *models.Place {
	name := n.Tags["name"]
	hasAddr := hasAddressTags(n.Tags)

	if name == "" && !hasAddr {
		return nil
	}

	if name == "" {
		housenumber := firstTag(n.Tags, "addr:housenumber")
		street := firstTag(n.Tags, "addr:street")
		if housenumber != "" && street != "" {
			name = housenumber + " " + street
		} else if street != "" {
			name = street
		} else {
			return nil
		}
	}

	class, typ := classType(n.Tags)
	return &models.Place{
		ID:         fmt.Sprintf("node/%d", n.ID),
		OSMID:      n.ID,
		OSMType:    "node",
		Name:       name,
		Address:    buildAddress(n.Tags),
		City:       firstTag(n.Tags, "addr:city", "city"),
		State:      firstTag(n.Tags, "addr:state", "state"),
		Postcode:   firstTag(n.Tags, "addr:postcode", "postcode"),
		Country:    firstTag(n.Tags, "addr:country", "country"),
		Lat:        n.Lat,
		Lon:        n.Lon,
		Class:      class,
		Type:       typ,
		Importance: placeImportance(n.Tags, class),
	}
}

// wayToPlace converts an OSM way to a Place if it has a name or address tags.
// Ways with addr:housenumber but no addr:street are also indexed.
// Ways carry no coordinates in the PBF, so lat/lon are left as 0 here; the
// way-coordinates phase (waycoords.go) resolves centroids after the import.
func wayToPlace(w *osmpbf.Way) *models.Place {
	if !isIndexedWay(w) {
		return nil
	}

	name := w.Tags["name"]
	if name == "" {
		housenumber := firstTag(w.Tags, "addr:housenumber")
		street := firstTag(w.Tags, "addr:street")
		if housenumber != "" && street != "" {
			name = housenumber + " " + street
		} else if housenumber != "" {
			name = housenumber
		} else if street != "" {
			name = street
		} else {
			return nil
		}
	}

	class, typ := classType(w.Tags)
	return &models.Place{
		ID:         fmt.Sprintf("way/%d", w.ID),
		OSMID:      w.ID,
		OSMType:    "way",
		Name:       name,
		Address:    buildAddress(w.Tags),
		City:       firstTag(w.Tags, "addr:city", "city"),
		State:      firstTag(w.Tags, "addr:state", "state"),
		Postcode:   firstTag(w.Tags, "addr:postcode", "postcode"),
		Country:    firstTag(w.Tags, "addr:country", "country"),
		Class:      class,
		Type:       typ,
		Importance: placeImportance(w.Tags, class),
	}
}

func firstTag(tags map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := tags[k]; ok && v != "" {
			return v
		}
	}
	return ""
}
