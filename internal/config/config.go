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
}

// Load reads configuration from environment variables.
func Load() *Config {
	return &Config{
		AllowedDomains: splitAndTrim(os.Getenv("ALLOWED_DOMAINS")),
		OSMDataURL:     getEnv("OSM_DATA_URL", "https://download.geofabrik.de/north-america/us-latest.osm.pbf"),
		OSMDataPath:    getEnv("OSM_DATA_PATH", "./data"),
		UpdateCron:     os.Getenv("UPDATE_CRON"),
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
