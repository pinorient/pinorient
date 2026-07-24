package osm

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
)

// bloomMagic identifies the on-disk bloom filter format, with a version
// suffix so future layout changes fail loudly instead of misreading bits.
const bloomMagic = "PBLM0001"

// bloomFilter is a space-efficient probabilistic set of OSM node IDs. It is
// used during the way-coordinates phase to select exactly the nodes referenced
// by indexed ways (~250M of the extract's ~1.1B nodes) without holding the
// full ID set in RAM: a 256MiB filter with 6 hash functions has a false
// positive rate of ~1-2% at that load, and false positives are harmless (they
// only store a few extra coordinates). False negatives never occur, so no
// indexed way ever loses its centroid to the filter.
type bloomFilter struct {
	bits []uint64
	m    uint64 // number of bits
	k    uint64 // number of hash functions
}

// newBloomFilter allocates a filter of at least sizeBytes bytes using k hash
// functions.
func newBloomFilter(sizeBytes int, k uint64) *bloomFilter {
	if sizeBytes < 1024 {
		sizeBytes = 1024
	}
	if k < 1 {
		k = 1
	}
	words := uint64(sizeBytes) / 8
	return &bloomFilter{bits: make([]uint64, words), m: words * 64, k: k}
}

// mix64 is the splitmix64 finalizer: a fast bijective hash that distributes
// sequential OSM IDs uniformly across the filter.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// positions calls fn with each of the k bit positions for id, using double
// hashing (h1 + i*h2) so only two real hashes are computed per ID.
func (b *bloomFilter) positions(id int64, fn func(pos uint64)) {
	h1 := mix64(uint64(id))
	h2 := mix64(h1) | 1 // odd, so the step cycles through the filter well
	for i := uint64(0); i < b.k; i++ {
		fn((h1 + i*h2) % b.m)
	}
}

func (b *bloomFilter) add(id int64) {
	b.positions(id, func(pos uint64) {
		b.bits[pos/64] |= 1 << (pos % 64)
	})
}

func (b *bloomFilter) maybeContains(id int64) bool {
	found := true
	b.positions(id, func(pos uint64) {
		if b.bits[pos/64]&(1<<(pos%64)) == 0 {
			found = false
		}
	})
	return found
}

// fillRatio returns the fraction of bits set (0-1). Useful for monitoring:
// near 1.0 the filter is saturated and false positives skyrocket.
func (b *bloomFilter) fillRatio() float64 {
	var set uint64
	for _, w := range b.bits {
		set += uint64(bits.OnesCount64(w))
	}
	return float64(set) / float64(b.m)
}

// save writes the filter to path atomically (via a .part rename) so an
// interrupted coordinate pass can resume without re-scanning all ways.
func (b *bloomFilter) save(path string) error {
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("failed to create bloom file: %w", err)
	}

	header := make([]byte, 24)
	copy(header, bloomMagic)
	binary.LittleEndian.PutUint64(header[8:], b.m)
	binary.LittleEndian.PutUint64(header[16:], b.k)
	if _, err := f.Write(header); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("failed to write bloom header: %w", err)
	}

	// Serialize the bit array in 1MiB chunks (the full filter is ~256MiB).
	const chunkWords = 1 << 17 // 128K uint64 = 1MiB
	buf := make([]byte, chunkWords*8)
	for start := 0; start < len(b.bits); start += chunkWords {
		end := start + chunkWords
		if end > len(b.bits) {
			end = len(b.bits)
		}
		n := end - start
		for i, w := range b.bits[start:end] {
			binary.LittleEndian.PutUint64(buf[i*8:], w)
		}
		if _, err := f.Write(buf[:n*8]); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("failed to write bloom bits: %w", err)
		}
	}

	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to close bloom file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to rename bloom file: %w", err)
	}
	return nil
}

// loadBloomFilter reads a filter previously written by save. Any format or
// size mismatch is an error — callers should treat it as "no saved filter"
// and rebuild.
func loadBloomFilter(path string) (*bloomFilter, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	header := make([]byte, 24)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("failed to read bloom header: %w", err)
	}
	if string(header[:8]) != bloomMagic {
		return nil, fmt.Errorf("bad bloom magic %q", header[:8])
	}
	m := binary.LittleEndian.Uint64(header[8:])
	k := binary.LittleEndian.Uint64(header[16:])
	if m == 0 || m%64 != 0 || m > 1<<40 || k == 0 || k > 64 {
		return nil, fmt.Errorf("invalid bloom parameters m=%d k=%d", m, k)
	}

	words := m / 64
	b := &bloomFilter{bits: make([]uint64, words), m: m, k: k}
	buf := make([]byte, 1<<20) // 1MiB read chunks
	for i := uint64(0); i < words; {
		remaining := (words - i) * 8
		n := uint64(len(buf))
		if remaining < n {
			n = remaining
		}
		// ReadFull also fails (ErrUnexpectedEOF) on truncated files.
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return nil, fmt.Errorf("failed to read bloom bits: %w", err)
		}
		for j := uint64(0); j < n; j += 8 {
			b.bits[i] = binary.LittleEndian.Uint64(buf[j:])
			i++
		}
	}
	return b, nil
}
