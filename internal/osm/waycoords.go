package osm

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/qedus/osmpbf"

	"github.com/pinorient/pinorient/internal/geocoder"
)

const (
	// bloomHashFunctions is the number of hash functions used by the way-node
	// bloom filter — near-optimal for the default 256MiB filter at ~250M IDs.
	bloomHashFunctions = 6
	// coordFlushSize is how many node coordinates are buffered before flushing
	// to _osm_node_coords during the coordinate collection pass.
	coordFlushSize = 5000
	// wayChunkSize is how many indexed ways are resolved per committed
	// transaction in the centroid pass.
	wayChunkSize = 2000
)

// resolveWayCoords computes a centroid for every indexed way from its member
// nodes and writes it to geocoder_places. Ways carry no coordinates in the
// PBF format (only node references), so without this phase 75% of the index
// (every building, POI, campus, and street mapped as a way) sits at 0,0 —
// unplottable, invisible to bbox filtering, and useless for reverse geocoding.
//
// Three crash-resumable phases work off the local extract (progress markers
// in _import_state; each phase is idempotent):
//
//  1. bloom — scan ways; add the node references of every indexed way to a
//     bloom filter (persisted to disk so a crash doesn't force a re-scan).
//  2. coords — scan nodes; store coordinates of bloom-positive nodes in
//     _osm_node_coords (~250M rows for the US extract, dropped afterwards).
//  3. resolve — scan ways again; compute each indexed way's centroid as the
//     average of its node coordinates and apply updates in committed chunks.
func (s *Scheduler) resolveWayCoords(ctx context.Context, path string) error {
	if !s.cfg.WayCoordsEnabled {
		return nil
	}

	wayDone, err := s.geo.GetImportState(ctx, geocoder.StateOSMWayCoordsDone)
	if err != nil {
		return fmt.Errorf("failed to check way coords state: %w", err)
	}
	if wayDone == "done" {
		return nil
	}

	nodeCoordsDone, err := s.geo.GetImportState(ctx, geocoder.StateOSMNodeCoordsDone)
	if err != nil {
		return fmt.Errorf("failed to check node coords state: %w", err)
	}

	bloomPath := filepath.Join(s.cfg.OSMDataPath, "way_nodes.bloom")

	if nodeCoordsDone != "done" {
		// The coordinate table plus WAL need several GB of scratch space.
		// Refuse to start when the disk cannot hold it: a full disk does
		// not just fail the import, it wedges SQLite for every request.
		if free, err := freeBytes(s.cfg.OSMDataPath); err == nil && free < wayCoordsMinFreeBytes {
			return fmt.Errorf("insufficient disk space for way coordinate resolution: %.1fGB free, need ~%dGB; free space or set WAY_COORDS_ENABLED=false",
				float64(free)/(1<<30), wayCoordsMinFreeBytes>>30)
		}

		bloom, err := s.buildWayNodeBloom(ctx, path, bloomPath)
		if err != nil {
			return err
		}
		if err := s.collectNodeCoords(ctx, path, bloom); err != nil {
			return err
		}
		bloom = nil // release the filter (~256MiB) before the resolve pass
		_ = os.Remove(bloomPath)
		if err := s.geo.SetImportState(ctx, geocoder.StateOSMNodeCoordsDone, "done"); err != nil {
			log.Printf("warning: failed to mark node coords complete: %v", err)
		}
		s.geo.Checkpoint(ctx)
	}

	if err := s.applyWayCentroids(ctx, path); err != nil {
		return err
	}

	if err := s.geo.DropNodeCoordTable(ctx); err != nil {
		log.Printf("warning: failed to drop node coords table: %v", err)
	}
	if err := s.geo.DropWayCoordUpdateTable(ctx); err != nil {
		log.Printf("warning: failed to drop way coord update table: %v", err)
	}
	if err := s.geo.DeleteImportState(ctx, geocoder.StateOSMWayCoordsOffset); err != nil {
		log.Printf("warning: failed to clear way coords offset: %v", err)
	}
	if err := s.geo.SetImportState(ctx, geocoder.StateOSMWayCoordsDone, "done"); err != nil {
		log.Printf("warning: failed to mark way coords complete: %v", err)
	}
	return nil
}

// buildWayNodeBloom returns a bloom filter containing the node references of
// every indexed way in the extract. A filter previously persisted to
// bloomPath is loaded instead of rebuilding (crash resume).
func (s *Scheduler) buildWayNodeBloom(ctx context.Context, pbfPath, bloomPath string) (*bloomFilter, error) {
	if bloom, err := loadBloomFilter(bloomPath); err == nil {
		log.Printf("way coords: loaded saved bloom filter from %s (%.1f%% full)", bloomPath, bloom.fillRatio()*100)
		return bloom, nil
	}

	bloomMB := s.cfg.WayCoordsBloomMB
	if bloomMB < 32 {
		bloomMB = 32
	}
	bloom := newBloomFilter(bloomMB*1024*1024, bloomHashFunctions)

	f, decoder, err := s.decodePBF(pbfPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	log.Printf("way coords: scanning ways for node references (%dMiB bloom filter)...", bloomMB)
	startTime := time.Now()
	var ways, refs int64
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		v, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode error: %w", err)
		}
		w, ok := v.(*osmpbf.Way)
		if !ok || !isIndexedWay(w) {
			continue
		}
		for _, id := range w.NodeIDs {
			bloom.add(id)
		}
		ways++
		refs += int64(len(w.NodeIDs))
		if ways%5000000 == 0 {
			log.Printf("way coords: bloom progress: %d ways, %d refs, elapsed=%s",
				ways, refs, time.Since(startTime).Round(time.Second))
		}
	}
	log.Printf("way coords: bloom built from %d ways / %d node refs in %s (filter %.1f%% full)",
		ways, refs, time.Since(startTime).Round(time.Second), bloom.fillRatio()*100)

	if err := bloom.save(bloomPath); err != nil {
		// Non-fatal: a crash just means rebuilding the filter on resume.
		log.Printf("warning: failed to persist bloom filter (it would be rebuilt after a crash): %v", err)
	}
	return bloom, nil
}

// collectNodeCoords scans the extract's nodes and stores the coordinates of
// every bloom-positive node in _osm_node_coords. Re-runs are idempotent
// (INSERT OR REPLACE), so an interrupted pass simply starts over.
func (s *Scheduler) collectNodeCoords(ctx context.Context, pbfPath string, bloom *bloomFilter) error {
	if err := s.geo.CreateNodeCoordTable(ctx); err != nil {
		return err
	}

	f, decoder, err := s.decodePBF(pbfPath)
	if err != nil {
		return err
	}
	defer f.Close()

	log.Printf("way coords: collecting node coordinates...")
	startTime := time.Now()
	buf := make([]geocoder.NodeCoord, 0, coordFlushSize)
	var scanned, kept int64

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := s.geo.BatchInsertNodeCoords(ctx, buf); err != nil {
			return err
		}
		kept += int64(len(buf))
		buf = buf[:0]
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
		n, ok := v.(*osmpbf.Node)
		if !ok {
			continue
		}
		scanned++
		if !bloom.maybeContains(n.ID) {
			continue
		}
		buf = append(buf, geocoder.NodeCoord{OSMID: n.ID, Lat: n.Lat, Lon: n.Lon})
		if len(buf) >= coordFlushSize {
			if err := flush(); err != nil {
				return err
			}
			// Checkpoint periodically so the WAL does not grow unbounded
			// over hundreds of millions of inserts (a full disk wedges the
			// whole server). No-op if another connection holds the lock.
			if kept%50000000 < coordFlushSize {
				s.geo.Checkpoint(ctx)
			}
			if kept%10000000 < coordFlushSize {
				log.Printf("way coords: node progress: scanned=%d kept=%d elapsed=%s",
					scanned, kept, time.Since(startTime).Round(time.Second))
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	log.Printf("way coords: collected %d node coords (scanned %d nodes) in %s",
		kept, scanned, time.Since(startTime).Round(time.Second))
	return nil
}

// applyWayCentroids scans the extract's ways, computes each indexed way's
// centroid from the collected node coordinates, and applies the updates in
// committed chunks. The last committed way OSM ID is persisted per chunk
// (StateOSMWayCoordsOffset); Geofabrik extracts emit ways in ascending ID
// order, so an interrupted pass resumes exactly where it stopped.
func (s *Scheduler) applyWayCentroids(ctx context.Context, pbfPath string) error {
	var after int64
	switch v, err := s.geo.GetImportState(ctx, geocoder.StateOSMWayCoordsOffset); {
	case err != nil:
		return fmt.Errorf("failed to read way coords progress: %w", err)
	case v != "":
		after, _ = strconv.ParseInt(v, 10, 64)
		log.Printf("way coords: resuming centroid pass after way ID %d", after)
	}

	// The updates touch lat/lon only, but an AFTER UPDATE trigger would fire
	// per row and rewrite each way's FTS entry (text is unchanged). Dropping
	// the triggers for the pass avoids tens of millions of pointless FTS
	// writes; they are recreated afterwards.
	if err := s.geo.DropFTSTriggers(ctx); err != nil {
		return err
	}
	defer func() {
		if err := s.geo.CreateFTSTriggers(ctx); err != nil {
			log.Printf("warning: failed to recreate FTS triggers: %v", err)
		}
	}()

	if err := s.geo.CreateWayCoordUpdateTable(ctx); err != nil {
		return err
	}

	f, decoder, err := s.decodePBF(pbfPath)
	if err != nil {
		return err
	}
	defer f.Close()

	log.Printf("way coords: resolving way centroids...")
	startTime := time.Now()
	var waysScanned, chunksCommitted, updatesApplied int64
	lastID := after

	type pendingWay struct {
		id      string
		nodeIDs []int64
	}
	chunk := make([]pendingWay, 0, wayChunkSize)

	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		idSet := make(map[int64]struct{}, len(chunk)*8)
		for _, pw := range chunk {
			for _, id := range pw.nodeIDs {
				idSet[id] = struct{}{}
			}
		}
		ids := make([]int64, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}

		coords, err := s.geo.LookupNodeCoords(ctx, ids)
		if err != nil {
			return err
		}

		updates := make([]geocoder.WayCoordUpdate, 0, len(chunk))
		for _, pw := range chunk {
			if lat, lon, ok := centroidOf(pw.nodeIDs, coords); ok {
				updates = append(updates, geocoder.WayCoordUpdate{ID: pw.id, Lat: lat, Lon: lon})
			}
			// ok=false: every node fell outside the extract's bounding
			// polygon (clipped ways at the borders) — leave lat/lon at 0.
		}
		if err := s.geo.ApplyWayCoordUpdates(ctx, updates, lastID); err != nil {
			return err
		}
		updatesApplied += int64(len(updates))
		chunksCommitted++
		chunk = chunk[:0]
		// Checkpoint every 500 chunks (~1M way updates) so the WAL stays
		// bounded over the ~40M-update pass (a full disk wedges the server).
		if chunksCommitted%500 == 0 {
			s.geo.Checkpoint(ctx)
		}
		return nil
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err() // committed chunks persist via the offset marker
		}
		v, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decode error: %w", err)
		}
		w, ok := v.(*osmpbf.Way)
		if !ok {
			continue
		}
		waysScanned++
		if w.ID <= after {
			continue // already committed in a previous run
		}
		lastID = w.ID
		if !isIndexedWay(w) {
			continue
		}
		chunk = append(chunk, pendingWay{id: fmt.Sprintf("way/%d", w.ID), nodeIDs: w.NodeIDs})
		if len(chunk) >= wayChunkSize {
			if err := flush(); err != nil {
				return err
			}
			if chunksCommitted%1000 == 0 {
				log.Printf("way coords: centroid progress: scanned=%d updated=%d last_way=%d elapsed=%s",
					waysScanned, updatesApplied, lastID, time.Since(startTime).Round(time.Second))
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	log.Printf("way coords: applied %d way centroids (scanned %d ways) in %s",
		updatesApplied, waysScanned, time.Since(startTime).Round(time.Second))
	return nil
}

// centroidOf averages the coordinates of the given node IDs. IDs missing from
// the lookup are ignored (ways clipped at extract boundaries). Returns
// ok=false when no coordinates were found at all.
func centroidOf(nodeIDs []int64, coords map[int64][2]float64) (lat, lon float64, ok bool) {
	var sumLat, sumLon float64
	var n int
	for _, id := range nodeIDs {
		c, found := coords[id]
		if !found {
			continue
		}
		sumLat += c[0]
		sumLon += c[1]
		n++
	}
	if n == 0 {
		return 0, 0, false
	}
	return sumLat / float64(n), sumLon / float64(n), true
}

// wayCoordsMinFreeBytes is the free disk space required before the node
// coordinate collection may start: ~7GB for the coordinate table of the US
// extract plus WAL headroom for the update pass.
const wayCoordsMinFreeBytes = 12 << 30 // 12GiB

// freeBytes returns the number of bytes available to unprivileged users on
// the filesystem holding path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// decodePBF opens path and starts an osmpbf decoder with the configured
// number of worker goroutines (defaulting to runtime.NumCPU).
func (s *Scheduler) decodePBF(path string) (*os.File, *osmpbf.Decoder, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open osm data: %w", err)
	}
	decoder := osmpbf.NewDecoder(f)
	workers := s.cfg.OSMDecoderWorkers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if err := decoder.Start(workers); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("failed to start osm decoder: %w", err)
	}
	return f, decoder, nil
}
