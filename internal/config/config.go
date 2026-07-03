package config

import (
	"os"
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

	// TIGERYear is the TIGER/Line year to download (e.g., "2024").
	TIGERYear string

	// TIGERCounties is a list of FIPS codes for counties to download
	// and import TIGER/Line ADDRFEAT data for (e.g., "25017" for Middlesex County, MA).
	TIGERCounties []string

	// TIGERForceReimport forces a full re-import of TIGER/Line data on startup,
	// even if the tiger_addr_ranges table already has data.
	TIGERForceReimport bool
}

// Load reads configuration from environment variables.
func Load() *Config {
	return &Config{
		AllowedDomains:     splitAndTrim(os.Getenv("ALLOWED_DOMAINS")),
		OSMDataURL:         getEnv("OSM_DATA_URL", "https://download.geofabrik.de/north-america/us-latest.osm.pbf"),
		OSMDataPath:        getEnv("OSM_DATA_PATH", "./data"),
		UpdateCron:         os.Getenv("UPDATE_CRON"),
		ForceReindex:       getEnv("FORCE_REINDEX", "") != "" && getEnv("FORCE_REINDEX", "") != "false" && getEnv("FORCE_REINDEX", "") != "0",
		TIGERYear:          getEnv("TIGER_YEAR", "2024"),
		TIGERCounties:      splitAndTrim(os.Getenv("TIGER_COUNTIES")),
		TIGERForceReimport: getEnv("TIGER_FORCE_REIMPORT", "") != "" && getEnv("TIGER_FORCE_REIMPORT", "") != "false" && getEnv("TIGER_FORCE_REIMPORT", "") != "0",
	}
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
