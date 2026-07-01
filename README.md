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
- **Admin UI visibility** — geocoder data is stored in a PocketBase collection, viewable from the admin dashboard
- **Batched imports** — OSM data is imported in transactional batches for performance
- **Progress logging** — import progress is logged with record count, elapsed time, and throughput

## Project Structure

```
.
├── cmd/geocoder-pb/        # Application entrypoint
├── internal/
│   ├── api/                # HTTP routes and middleware
│   ├── config/             # Environment-based configuration
│   ├── db/                 # Schema migrations (PocketBase collection + FTS5)
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
| `FORCE_REINDEX` | Force full re-import on startup (`true`/`1` to enable) | *(empty = disabled)* |

## Data Storage

Geocoder data is stored in PocketBase's main SQLite database (`pb_data/data.db`):

- `geocoder_places` — a **PocketBase collection** containing the indexed place records. This collection is visible and manageable from the PocketBase admin UI at `/_/collections/geocoder_places`.
- `geocoder_places_fts` — an FTS5 virtual table for full-text address search. This is a raw SQL table (not a collection) because FTS5 virtual tables are not directly supported as PocketBase collection types. It is kept in sync with `geocoder_places` via SQL triggers.

The schema is created automatically on first startup by `internal/db/migrations.go`.

## Initial Data Fetch

On first startup, the app checks whether `geocoder_places` is empty. If it is, it automatically downloads the configured OSM extract and indexes it. The import runs in the background so the server starts immediately.

### Recovering from Interrupted Imports

If an import was interrupted (e.g. the process was killed), the index will be partially populated. On restart, the app sees existing records and skips indexing. To force a full re-import:

```bash
FORCE_REINDEX=true ./geocoder-pb serve
```

This clears the existing index and re-imports all OSM data from the downloaded PBF file.

### Import Performance

The importer uses batched transactions (5,000 records per batch) for dramatically faster inserts compared to one-by-one inserts. Progress is logged every 50,000 records with count, elapsed time, and records/second throughput.

## Admin Access

To view and manage geocoder data from the PocketBase admin UI:

```bash
./geocoder-pb superuser upsert admin@example.com password
```

Then navigate to `http://127.0.0.1:8090/_/` and log in. The `geocoder_places` collection will appear in the collections list, where you can browse, search, and manage records.

## Usage

```bash
# Build
go build .

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