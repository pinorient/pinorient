package osm

import (
	"context"
	"fmt"
	"io"
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
func (p *Parser) Parse(ctx context.Context, r io.Reader) error {
	decoder := osmpbf.NewDecoder(r)
	if err := decoder.Start(0); err != nil {
		return fmt.Errorf("failed to start osm decoder: %w", err)
	}

	startTime := time.Now()
	totalIndexed := 0
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
		}

		if place != nil {
			buffer = append(buffer, place)

			if len(buffer) >= batchBufferSize {
				if err := flush(); err != nil {
					return err
				}

				// Log progress at regular intervals.
				if totalIndexed%progressInterval < batchBufferSize {
					p.geo.LogProgress(totalIndexed, startTime)
				}
			}
		}
	}

	// Flush any remaining buffered places.
	if err := flush(); err != nil {
		return err
	}

	// Rebuild the FTS index to ensure all data is searchable.
	p.geo.LogProgress(totalIndexed, startTime)
	if err := p.geo.RebuildFTS(ctx); err != nil {
		return fmt.Errorf("failed to rebuild FTS index after import: %w", err)
	}

	elapsed := time.Since(startTime)
	p.geo.AppLogger().Info("osm import complete",
		"total_indexed", totalIndexed,
		"elapsed", elapsed.Round(time.Second).String(),
		"rate", fmt.Sprintf("%.0f/s", float64(totalIndexed)/elapsed.Seconds()),
	)

	return nil
}

func nodeToPlace(n *osmpbf.Node) *models.Place {
	name := n.Tags["name"]
	if name == "" {
		return nil
	}

	return &models.Place{
		ID:       fmt.Sprintf("node/%d", n.ID),
		OSMID:    n.ID,
		OSMType:  "node",
		Name:     name,
		Address:  firstTag(n.Tags, "addr:street", "street"),
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

func wayToPlace(w *osmpbf.Way) *models.Place {
	name := w.Tags["name"]
	if name == "" {
		return nil
	}

	// Ways do not carry coordinates directly in this library; centroid
	// computation requires resolving referenced nodes. For the scaffold we
	// store a placeholder and leave centroid resolution for a later pass.
	return &models.Place{
		ID:       fmt.Sprintf("way/%d", w.ID),
		OSMID:    w.ID,
		OSMType:  "way",
		Name:     name,
		Address:  firstTag(w.Tags, "addr:street", "street"),
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
