# geocoder-pb

A pure Go / SQLite geocoder built on [PocketBase](https://pocketbase.io/) and OpenStreetMap (OSM) data.

## Overview

`geocoder-pb` translates written addresses into map coordinates using OpenStreetMap data stored locally in SQLite with FTS5 full-text search. It is designed as a lightweight, self-hosted alternative to the Photon API, avoiding the overhead of Java, Elasticsearch, and Docker orchestration.

## Features

- PocketBase (v0.39.0+) extended as a Go framework
- SQLite with FTS5 for fast address search
- OSM PBF parsing via `qedus/osmpbf`
- Forward geocoding (`/api/geocoder/search?q=...`)
- Reverse geocoding (`/api/geocoder/reverse?lat=...&lon=...`)
- Domain-based access restriction with wildcard support (`*.mysite.com`)
- Periodic OSM data refresh via cron expression

## Project Structure

```
.
├── cmd/geocoder-pb/        # Application entrypoint
├── internal/
│   ├── api/                # HTTP routes and middleware
│   ├── config/             # Environment-based configuration
│   ├── db/                 # SQLite migrations and schema
│   ├── geocoder/           # Search / reverse / indexing logic
│   ├── models/             # Data models
│   └── osm/                # OSM PBF parsing and refresh scheduler
├── go.mod
├── go.sum
├── .gitignore
└── README.md
```

## Configuration

Configuration is read from environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `ALLOWED_DOMAINS` | Comma-separated allowed domains (supports `*.mysite.com`) | *(empty = public)* |
| `OSM_DATA_URL` | URL to fetch OSM PBF data | `https://download.geofabrik.de/north-america/us-latest.osm.pbf` |
| `OSM_DATA_PATH` | Local directory for OSM data | `./data` |
| `UPDATE_CRON` | Cron expression for periodic refresh | *(empty = disabled)* |

## Usage

```bash
# Build
go build ./cmd/geocoder-pb

# Run
./geocoder-pb serve

# Search
GET /api/geocoder/search?q=1600+Pennsylvania+Avenue+NW,+Washington,+DC

# Reverse
GET /api/geocoder/reverse?lat=38.8977&lon=-77.0365
```

## Data Refresh

Set `UPDATE_CRON` to a valid cron expression to automatically download and re-index OSM data on a schedule. For example:

```bash
UPDATE_CRON="0 3 * * 0" ./geocoder-pb serve
```

## License

MIT
