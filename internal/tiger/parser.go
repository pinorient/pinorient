package tiger

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonas-p/go-shp"

	"github.com/sellography/geocoder-pb/internal/geocoder"
)

const defaultBatchSize = 2000
const progressInterval = 50000

type Parser struct {
	geo       *geocoder.Geocoder
	batchSize int
}

func NewParser(geo *geocoder.Geocoder, batchSize int) *Parser {
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	return &Parser{geo: geo, batchSize: batchSize}
}

func (p *Parser) ParseDir(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read tiger directory: %w", err)
	}

	startTime := time.Now()
	totalImported := 0

	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !strings.HasSuffix(entry.Name(), ".shp") {
			continue
		}

		shpPath := filepath.Join(dir, entry.Name())
		log.Printf("parsing tiger file: %s", entry.Name())

		count, err := p.parseFile(ctx, shpPath)
		if err != nil {
			return fmt.Errorf("failed to parse %s: %w", entry.Name(), err)
		}

		totalImported += count
		log.Printf("  imported %d address ranges (total: %d, elapsed: %s)",
			count, totalImported, time.Since(startTime).Round(time.Second))
	}

	log.Printf("tiger import complete: total=%d elapsed=%s",
		totalImported, time.Since(startTime).Round(time.Second))
	return nil
}

func (p *Parser) parseFile(ctx context.Context, shpPath string) (int, error) {
	shape, err := shp.Open(shpPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open shapefile: %w", err)
	}
	defer shape.Close()

	imported := 0
	buffer := make([]*geocoder.AddrRange, 0, p.batchSize)

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		saved, err := p.geo.BatchUpsertAddrRanges(ctx, buffer, p.batchSize)
		if err != nil {
			return err
		}
		imported += saved
		buffer = buffer[:0]
		return nil
	}

	for i := 0; shape.Next(); i++ {
		if ctx.Err() != nil {
			_ = flush()
			return imported, ctx.Err()
		}

		_, shapeObj := shape.Shape()
		fullName := shape.ReadAttribute(i, 6)
		if fullName == "" {
			continue
		}

		lat, lon := getShapeMidpoint(shapeObj)

		lfrom := shape.ReadAttribute(i, 7)
		lto := shape.ReadAttribute(i, 8)
		zipl := shape.ReadAttribute(i, 11)
		parityl := shape.ReadAttribute(i, 15)
		if ar := makeAddrRange(fullName, lfrom, lto, parityl, zipl, "L", lat, lon); ar != nil {
			buffer = append(buffer, ar)
		}

		rfrom := shape.ReadAttribute(i, 9)
		rto := shape.ReadAttribute(i, 10)
		zipr := shape.ReadAttribute(i, 12)
		parityr := shape.ReadAttribute(i, 16)
		if ar := makeAddrRange(fullName, rfrom, rto, parityr, zipr, "R", lat, lon); ar != nil {
			buffer = append(buffer, ar)
		}

		if len(buffer) >= p.batchSize {
			if err := flush(); err != nil {
				return imported, err
			}
			if imported%progressInterval < p.batchSize {
				log.Printf("tiger import progress: imported=%d", imported)
			}
		}
	}

	if err := flush(); err != nil {
		return imported, err
	}
	return imported, nil
}

func makeAddrRange(fullName, fromHN, toHN, parity, zip, side string, lat, lon float64) *geocoder.AddrRange {
	from, err := strconv.Atoi(strings.TrimSpace(fromHN))
	if err != nil || from == 0 {
		return nil
	}
	to, err := strconv.Atoi(strings.TrimSpace(toHN))
	if err != nil || to == 0 {
		return nil
	}
	p := strings.TrimSpace(parity)
	if p != "E" && p != "O" && p != "B" {
		// Infer parity from the from house number when TIGER doesn't specify.
		if from%2 == 0 {
			p = "E"
		} else {
			p = "O"
		}
	}
	return &geocoder.AddrRange{
		FullName: fullName,
		FromHN:   from,
		ToHN:     to,
		Parity:   p,
		ZIP:      strings.TrimSpace(zip),
		Side:     side,
		Lat:      lat,
		Lon:      lon,
	}
}

func getShapeMidpoint(shapeObj shp.Shape) (float64, float64) {
	switch g := shapeObj.(type) {
	case *shp.PolyLine:
		if len(g.Points) == 0 {
			return 0, 0
		}
		mid := len(g.Points) / 2
		if mid >= len(g.Points) {
			mid = 0
		}
		return g.Points[mid].Y, g.Points[mid].X
	}
	return 0, 0
}
