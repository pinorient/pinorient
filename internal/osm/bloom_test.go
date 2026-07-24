package osm

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestBloomFilterNoFalseNegatives(t *testing.T) {
	b := newBloomFilter(1<<20, bloomHashFunctions) // 1MiB
	const n = 10000
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = rand.Int63()
		b.add(ids[i])
	}
	for _, id := range ids {
		if !b.maybeContains(id) {
			t.Fatalf("false negative for id %d (bloom filters must never produce them)", id)
		}
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	b := newBloomFilter(1<<20, bloomHashFunctions) // 1MiB
	const inserted = 10000
	for i := 0; i < inserted; i++ {
		b.add(int64(1000000 + i))
	}

	// Probe with sequential IDs the filter has never seen.
	var fp int
	const probes = 100000
	for i := 0; i < probes; i++ {
		if b.maybeContains(int64(900000000 + i)) {
			fp++
		}
	}
	rate := float64(fp) / probes
	if rate > 0.02 {
		t.Errorf("false positive rate %.4f too high after %d inserts, want < 0.02", rate, inserted)
	}
	if r := b.fillRatio(); r <= 0 || r >= 1 {
		t.Errorf("suspicious fill ratio %f", r)
	}
}

func TestBloomSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.bloom")

	b := newBloomFilter(64*1024, bloomHashFunctions)
	ids := []int64{42, 1234567890123, 7, 9000000000000}
	for _, id := range ids {
		b.add(id)
	}
	if err := b.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadBloomFilter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.m != b.m || loaded.k != b.k {
		t.Errorf("params mismatch: got m=%d k=%d, want m=%d k=%d", loaded.m, loaded.k, b.m, b.k)
	}
	for _, id := range ids {
		if !loaded.maybeContains(id) {
			t.Errorf("loaded filter lost id %d", id)
		}
	}
	if loaded.fillRatio() != b.fillRatio() {
		t.Errorf("fill ratio changed on round trip: %f -> %f", b.fillRatio(), loaded.fillRatio())
	}
}

func TestLoadBloomFilterCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.bloom")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBloomFilter(path); err == nil {
		t.Error("expected error loading corrupt bloom file")
	}
}
