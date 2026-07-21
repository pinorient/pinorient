package config

import "testing"

func TestLoadImportDefaults(t *testing.T) {
	// Ensure a clean environment so defaults are exercised.
	t.Setenv("FTS_REBUILD_CHUNK", "")
	t.Setenv("TIGER_KEEP_DATA", "")
	t.Setenv("OSM_KEEP_DATA", "")
	t.Setenv("IMPORT_BATCH_SIZE", "")
	t.Setenv("OSM_DECODER_WORKERS", "")
	t.Setenv("SERIALIZE_IMPORTS", "")

	cfg := Load()

	if cfg.FTSRebuildChunkSize != 250000 {
		t.Errorf("FTSRebuildChunkSize = %d, want 250000", cfg.FTSRebuildChunkSize)
	}
	if cfg.TIGERKeepData {
		t.Error("TIGERKeepData should default to false (delete source files after import)")
	}
	if !cfg.OSMKeepData {
		t.Error("OSMKeepData should default to true")
	}
	if cfg.ImportBatchSize != 2000 {
		t.Errorf("ImportBatchSize = %d, want 2000", cfg.ImportBatchSize)
	}
	if cfg.OSMDecoderWorkers != 2 {
		t.Errorf("OSMDecoderWorkers = %d, want 2", cfg.OSMDecoderWorkers)
	}
	if !cfg.SerializeImports {
		t.Error("SerializeImports should default to true")
	}
}

func TestLoadImportOverrides(t *testing.T) {
	t.Setenv("FTS_REBUILD_CHUNK", "50000")
	t.Setenv("TIGER_KEEP_DATA", "1")
	t.Setenv("OSM_KEEP_DATA", "false")
	t.Setenv("IMPORT_BATCH_SIZE", "5000")

	cfg := Load()

	if cfg.FTSRebuildChunkSize != 50000 {
		t.Errorf("FTSRebuildChunkSize = %d, want 50000", cfg.FTSRebuildChunkSize)
	}
	if !cfg.TIGERKeepData {
		t.Error("TIGERKeepData should be true when TIGER_KEEP_DATA=1")
	}
	if cfg.OSMKeepData {
		t.Error("OSMKeepData should be false when OSM_KEEP_DATA=false")
	}
	if cfg.ImportBatchSize != 5000 {
		t.Errorf("ImportBatchSize = %d, want 5000", cfg.ImportBatchSize)
	}
}
