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

	"github.com/sellography/geocoder-pb/internal/geocoder"
	"github.com/sellography/geocoder-pb/internal/models"
)

// batchBufferSize is the number of places to buffer before flushing to the database.
const batchBufferSize = 5000

// progressInterval is how often (in records) to log import progress.
const progressInterval = 50000

// Parser reads OSM PBF data and indexes relevant places.
type Parser struct {
	geo *geocoder.Geocoder
}

// NewParser creates a new OSM parser.
func NewParser(geo *geocoder.Geocoder) *Parser {
	return &Parser{geo: geo}
}

// Parse reads OSM PBF data from r and indexes addressable places.
// Places are buffered and inserted in batched transactions for performance.
// Both named places and address-tagged features (buildings, address points)
// are indexed to support street address autocomplete.
//
// FTS triggers are temporarily dropped during import to avoid per-row FTS
// overhead, and the FTS index is rebuilt once at the end.
func (p *Parser) Parse(ctx context.Context, r io.Reader) error {
	// Drop FTS triggers to speed up bulk inserts.
	if err := p.geo.DropFTSTriggers(ctx); err != nil {
		return fmt.Errorf("failed to drop FTS triggers: %w", err)
	}
	defer func() {
		// Always try to recreate triggers.
		if err := p.geo.CreateFTSTriggers(ctx); err != nil {
			log.Printf("warning: failed to recreate FTS triggers: %v", err)
		}
	}()

	decoder := osmpbf.NewDecoder(r)
	// Use multiple goroutines for faster decoding.
	if err := decoder.Start(runtime.NumCPU()); err != nil {
		return fmt.Errorf("failed to start osm decoder: %w", err)
	}

	startTime := time.Now()
	totalIndexed := 0
	skipped := 0
	buffer := make([]*models.Place, 0, batchBufferSize)

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}

		saved, err := p.geo.BatchUpsertPlaces(ctx, buffer, batchBufferSize)
		if err != nil {
			return err
		}
		totalIndexed += saved
		buffer = buffer[:0] // reset buffer while keeping capacity
		return nil
	}

	for {
		if ctx.Err() != nil {
			_ = flush() // try to flush before returning
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
			// Relations are not currently indexed.
			continue
		}

		if place != nil {
			buffer = append(buffer, place)

			if len(buffer) >= batchBufferSize {
				if err := flush(); err != nil {
					return err
				}

				// Log progress at regular intervals.
				if totalIndexed%progressInterval < batchBufferSize {
					elapsed := time.Since(startTime)
					log.Printf("osm import progress: indexed=%d skipped=%d elapsed=%s rate=%.0f/s",
						totalIndexed, skipped, elapsed.Round(time.Second), float64(totalIndexed)/elapsed.Seconds())
				}
			}
		} else {
			skipped++
		}
	}

	// Flush any remaining buffered places.
	if err := flush(); err != nil {
		return err
	}

	// Rebuild the FTS index in one shot (much faster than per-row triggers).
	elapsed := time.Since(startTime)
	log.Printf("osm import: all records inserted, rebuilding FTS index... (indexed=%d skipped=%d elapsed=%s)",
		totalIndexed, skipped, elapsed.Round(time.Second))
	if err := p.geo.RebuildFTS(ctx); err != nil {
		return fmt.Errorf("failed to rebuild FTS index after import: %w", err)
	}

	log.Printf("osm import complete: indexed=%d skipped=%d elapsed=%s rate=%.0f/s",
		totalIndexed, skipped, elapsed.Round(time.Second), float64(totalIndexed)/elapsed.Seconds())

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

// nodeToPlace converts an OSM node to a Place if it has a name or address tags.
func nodeToPlace(n *osmpbf.Node) *models.Place {
	name := n.Tags["name"]
	hasAddr := hasAddressTags(n.Tags)

	// Skip if neither a name nor address tags are present.
	if name == "" && !hasAddr {
		return nil
	}

	// For nodes with only address tags, build a name from the address.
	if name == "" {
		housenumber := firstTag(n.Tags, "addr:housenumber")
		street := firstTag(n.Tags, "addr:street")
		if housenumber != "" && street != "" {
			name = housenumber + " " + street
		} else if street != "" {
			name = street
		} else {
			// Has addr: tags but not street/housenumber — skip.
			return nil
		}
	}

	return &models.Place{
		ID:       fmt.Sprintf("node/%d", n.ID),
		OSMID:    n.ID,
		OSMType:  "node",
		Name:     name,
		Address:  buildAddress(n.Tags),
		City:     firstTag(n.Tags, "addr:city", "city"),
		State:    firstTag(n.Tags, "addr:state", "state"),
		Postcode: firstTag(n.Tags, "addr:postcode", "postcode"),
		Country:  firstTag(n.Tags, "addr:country", "country"),
		Lat:      n.Lat,
		Lon:      n.Lon,
		Class:    n.Tags["class"],
		Type:     n.Tags["type"],
	}
}

// wayToPlace converts an OSM way to a Place if it has a name or address tags.
func wayToPlace(w *osmpbf.Way) *models.Place {
	name := w.Tags["name"]
	hasAddr := hasAddressTags(w.Tags)

	// Skip if neither a name nor address tags are present.
	if name == "" && !hasAddr {
		return nil
	}

	// For ways with only address tags, build a name from the address.
	if name == "" {
		housenumber := firstTag(w.Tags, "addr:housenumber")
		street := firstTag(w.Tags, "addr:street")
		if housenumber != "" && street != "" {
			name = housenumber + " " + street
		} else if street != "" {
			name = street
		} else {
			// Has addr: tags but not street/housenumber — skip.
			return nil
		}
	}

	// Ways do not carry coordinates directly in this library; centroid
	// computation requires resolving referenced nodes. For the scaffold we
	// store a placeholder and leave centroid resolution for a later pass.
	return &models.Place{
		ID:       fmt.Sprintf("way/%d", w.ID),
		OSMID:    w.ID,
		OSMType:  "way",
		Name:     name,
		Address:  buildAddress(w.Tags),
		City:     firstTag(w.Tags, "addr:city", "city"),
		State:    firstTag(w.Tags, "addr:state", "state"),
		Postcode: firstTag(w.Tags, "addr:postcode", "postcode"),
		Country:  firstTag(w.Tags, "addr:country", "country"),
		Class:    w.Tags["class"],
		Type:     w.Tags["type"],
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
