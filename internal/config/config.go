package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds application configuration.
type Config struct {
	// AllowedDomains is a list of domains permitted to access the geocoder API.
	// An empty list means public access. Supports wildcard prefixes like *.mysite.com.
	AllowedDomains []string

	// OSMDataURL is the URL to fetch OSM PBF data from (e.g. Geofabrik US extract).
	OSMDataURL string

	// OSMDataPath is the local path to store downloaded OSM PBF files.
	OSMDataPath string

	// UpdateCron is a cron expression for periodic OSM data refresh.
	// Leave empty to disable scheduled updates.
	UpdateCron string

	// ForceReindex forces a full re-import of OSM data on startup,
	// even if the geocoder index is already populated.
	// Useful for recovering from interrupted imports or schema changes.
	ForceReindex bool

	// TIGERYear is the TIGER/Line year to download (e.g., "2025").
	TIGERYear string

	// TIGERCounties is a list of FIPS codes for counties to download
	// and import TIGER/Line ADDRFEAT data for (e.g., "25017" for Middlesex County, MA).
	// If TIGERAllCounties is true, this is ignored and all US counties are imported.
	TIGERCounties []string

	// TIGERAllCounties enables importing TIGER/Line ADDRFEAT data for all US counties.
	// This downloads ~3,200 county shapefiles (~50GB total) and may take several hours.
	// Defaults to true. Set TIGER_ALL_COUNTIES=false/0 to disable and use TIGER_COUNTIES instead.
	TIGERAllCounties bool

	// TIGERForceReimport forces a full re-import of TIGER/Line data on startup,
	// even if the tiger_addr_ranges table already has data.
	TIGERForceReimport bool

	// ImportBatchSize controls how many records are buffered before flushing to
	// the database during OSM and TIGER imports. Smaller values reduce peak
	// memory usage at the cost of more frequent DB writes.
	// Defaults to 2000 (safe for 2GB RAM servers). Increase to 5000+ on
	// machines with 8GB+ RAM for faster imports.
	ImportBatchSize int

	// OSMDecoderWorkers controls the number of goroutines used by the OSM PBF
	// decoder. Each worker consumes additional memory. On low-memory servers
	// (2GB RAM), set this to 1-2. Defaults to 2.
	// Set to 0 to use runtime.NumCPU() (not recommended for low-memory servers).
	OSMDecoderWorkers int

	// SerializeImports runs OSM and TIGER imports sequentially instead of
	// concurrently. This halves peak memory usage at the cost of longer
	// total wall-clock time (though imports are I/O-bound, so the slowdown
	// is minimal). Defaults to true for safety on low-memory servers.
	// Set SERIALIZE_IMPORTS=false to run imports concurrently (8GB+ RAM recommended).
	SerializeImports bool

	// FTSRebuildChunkSize controls how many rows are indexed per committed
	// transaction when (re)building FTS5 indexes. Chunked rebuilds keep WAL
	// growth and peak memory bounded on low-RAM servers and can resume from
	// the last committed chunk after a crash. Defaults to 250000.
	FTSRebuildChunkSize int

	// TIGERKeepData keeps downloaded TIGER/Line ZIPs and extracted shapefiles
	// on disk after a county has been imported. Defaults to false (delete
	// after import) because the full US dataset is ~100GB unpacked, which
	// does not fit on small VPS disks. Import state is tracked in the
	// database, so deleting the files does not cause re-imports.
	TIGERKeepData bool

	// OSMKeepData keeps the downloaded OSM PBF file after a successful
	// import. Defaults to true so scheduled refreshes and forced re-indexes
	// don't have to re-download the (multi-GB) extract. Set OSM_KEEP_DATA=false
	// to delete the file after import on space-constrained servers.
	OSMKeepData bool
}

// Load reads configuration from environment variables.
func Load() *Config {
	return &Config{
		AllowedDomains: splitAndTrim(os.Getenv("ALLOWED_DOMAINS")),
		OSMDataURL:     getEnv("OSM_DATA_URL", "https://download.geofabrik.de/north-america/us-latest.osm.pbf"),
		OSMDataPath:    getEnv("OSM_DATA_PATH", "./pb_data/geo_data"),
		UpdateCron:     os.Getenv("UPDATE_CRON"),
		ForceReindex:   getEnv("FORCE_REINDEX", "") != "" && getEnv("FORCE_REINDEX", "") != "false" && getEnv("FORCE_REINDEX", "") != "0",
		TIGERYear:      getEnv("TIGER_YEAR", "2025"),
		TIGERCounties:  splitAndTrim(os.Getenv("TIGER_COUNTIES")),
		// TIGER_ALL_COUNTIES defaults to true. Set to false/0 to disable.
		TIGERAllCounties:   getEnv("TIGER_ALL_COUNTIES", "true") != "false" && getEnv("TIGER_ALL_COUNTIES", "true") != "0",
		TIGERForceReimport: getEnv("TIGER_FORCE_REIMPORT", "") != "" && getEnv("TIGER_FORCE_REIMPORT", "") != "false" && getEnv("TIGER_FORCE_REIMPORT", "") != "0",
		ImportBatchSize:    getEnvInt("IMPORT_BATCH_SIZE", 2000),
		OSMDecoderWorkers:  getEnvInt("OSM_DECODER_WORKERS", 2),
		// SERIALIZE_IMPORTS defaults to true for safety on low-memory servers.
		SerializeImports:    getEnv("SERIALIZE_IMPORTS", "true") != "false" && getEnv("SERIALIZE_IMPORTS", "true") != "0",
		FTSRebuildChunkSize: getEnvInt("FTS_REBUILD_CHUNK", 250000),
		// TIGER_KEEP_DATA defaults to false: source files are deleted after
		// each county is imported to keep disk usage bounded (~100GB otherwise).
		TIGERKeepData: getEnv("TIGER_KEEP_DATA", "") == "true" || getEnv("TIGER_KEEP_DATA", "") == "1",
		// OSM_KEEP_DATA defaults to true so re-indexes don't re-download the extract.
		OSMKeepData: getEnv("OSM_KEEP_DATA", "true") != "false" && getEnv("OSM_KEEP_DATA", "true") != "0",
	}
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
