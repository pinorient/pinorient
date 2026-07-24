package osm

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pinorient/pinorient/internal/config"
)

func TestCentroidOf(t *testing.T) {
	coords := map[int64][2]float64{
		1: {43.0, -70.0},
		2: {43.002, -70.002},
		3: {43.004, -70.004},
	}
	lat, lon, ok := centroidOf([]int64{1, 2, 3}, coords)
	if !ok {
		t.Fatal("centroidOf returned ok=false, want true")
	}
	wantLat := (43.0 + 43.002 + 43.004) / 3
	wantLon := (-70.0 + -70.002 + -70.004) / 3
	if math.Abs(lat-wantLat) > 1e-9 || math.Abs(lon-wantLon) > 1e-9 {
		t.Errorf("centroidOf = (%f, %f), want (%f, %f)", lat, lon, wantLat, wantLon)
	}
}

func TestCentroidOfMissingNodes(t *testing.T) {
	coords := map[int64][2]float64{1: {43.0, -70.0}}

	// Missing IDs are ignored; the average uses only found nodes.
	lat, lon, ok := centroidOf([]int64{1, 999, 998}, coords)
	if !ok {
		t.Fatal("centroidOf with one found node returned ok=false")
	}
	if lat != 43.0 || lon != -70.0 {
		t.Errorf("centroidOf = (%f, %f), want (43.0, -70.0)", lat, lon)
	}

	// No found nodes at all → ok=false (caller leaves lat/lon at 0).
	if _, _, ok := centroidOf([]int64{999}, coords); ok {
		t.Error("centroidOf with no found nodes returned ok=true, want false")
	}
	if _, _, ok := centroidOf(nil, coords); ok {
		t.Error("centroidOf with no node IDs returned ok=true, want false")
	}
}

// TestBloomBuildSmoke builds the way-node bloom filter from a real OSM
// extract. It is skipped unless OSM_SMOKE_PBF points at a .osm.pbf file;
// OSM_SMOKE_BLOOM_MB overrides the filter size (default 256MiB).
func TestBloomBuildSmoke(t *testing.T) {
	pbf := os.Getenv("OSM_SMOKE_PBF")
	if pbf == "" {
		t.Skip("set OSM_SMOKE_PBF to a .osm.pbf file to run this smoke test")
	}
	mb, _ := strconv.Atoi(os.Getenv("OSM_SMOKE_BLOOM_MB"))

	s := &Scheduler{cfg: &config.Config{
		OSMDecoderWorkers: 4,
		WayCoordsBloomMB:  mb,
		WayCoordsEnabled:  true,
	}}
	bloom, err := s.buildWayNodeBloom(context.Background(), pbf, filepath.Join(t.TempDir(), "way_nodes.bloom"))
	if err != nil {
		t.Fatalf("buildWayNodeBloom: %v", err)
	}
	t.Logf("bloom fill ratio: %.2f%%", bloom.fillRatio()*100)
	if bloom.fillRatio() == 0 {
		t.Error("bloom filter is empty after scanning the extract")
	}
}
