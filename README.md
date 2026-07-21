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
- **Bounding box filtering** — limit search/autocomplete results to a geographic area (`bbox` parameter)
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

Configuration is read from environment variables. A `.env` file is **optional** — if present, it is loaded automatically (via `godotenv`), but all settings have sensible defaults and the app will start and auto-download data without any configuration file. This means you can deploy the binary to a server and simply run `./geocoder-pb serve` with no `.env` file or environment variables set.

> **Note:** On a public-facing deployment, you may see 404 requests in the logs for paths like `/.env`, `/config.js`, `/api/config`, etc. These are automated scanners probing for exposed secrets. The server correctly returns 404 for all of them — no sensitive files are served.

| Variable | Description | Default |
|----------|-------------|---------|
| `ALLOWED_DOMAINS` | Comma-separated allowed domains (supports `*.mysite.com`) | *(empty = public)* |
| `OSM_DATA_URL` | URL to fetch OSM PBF data | `https://download.geofabrik.de/north-america/us-latest.osm.pbf` |
| `OSM_DATA_PATH` | Local directory for OSM data (within PocketBase data dir) | `./pb_data/geo_data` |
| `UPDATE_CRON` | Cron expression for periodic refresh | *(empty = disabled)* |
| `FORCE_REINDEX` | Force full re-import on startup (`true`/`1` to enable) | *(empty = disabled)* |
| `TIGER_YEAR` | Year of TIGER/Line data to download | `2025` |
| `TIGER_ALL_COUNTIES` | Import TIGER/Line ADDRFEAT data for all US counties (~3,200 counties) | `true` |
| `TIGER_COUNTIES` | Comma-separated county FIPS codes (only used when `TIGER_ALL_COUNTIES=false`) | *(empty)* |
| `TIGER_FORCE_REIMPORT` | Force re-import of TIGER/Line data on startup | *(empty = disabled)* |
| `TIGER_KEEP_DATA` | Keep TIGER ZIPs/shapefiles after import (`false` deletes them per county) | `false` |
| `OSM_KEEP_DATA` | Keep the downloaded OSM PBF after a successful import | `true` |
| `IMPORT_BATCH_SIZE` | Records buffered per DB transaction during imports | `2000` |
| `OSM_DECODER_WORKERS` | Goroutines for OSM PBF decoding (each uses extra memory) | `2` |
| `SERIALIZE_IMPORTS` | Run OSM and TIGER imports sequentially (halves peak memory) | `true` |
| `FTS_REBUILD_CHUNK` | Rows indexed per transaction during FTS index rebuilds | `250000` |
| `DB_CACHE_SIZE` | SQLite page cache per database, in KB | `65536` (64MB) |
| `DB_MMAP_SIZE` | SQLite mmap size in bytes (`0` = disabled) | `0` |
| `GOMEMLIMIT` | Standard Go heap limit (e.g. `1800MiB`); overrides `GO_MEM_LIMIT_MB` | `1500MiB` |
| `GO_MEM_LIMIT_MB` | Go heap cap in MiB, applied only when `GOMEMLIMIT` is unset | `1500` |

### Running on a small VPS (2GB RAM)

The defaults are tuned so a full US import completes on a 2GB server:

- **Chunked, resumable index builds.** The FTS5 indexes are built in committed chunks of `FTS_REBUILD_CHUNK` rows instead of one giant transaction. WAL growth and tokenizer memory stay bounded, and if the process is killed, the rebuild resumes from the last committed chunk instead of starting over.
- **Crash-safe imports.** Progress is tracked in the `_import_state` table. If the server dies mid-import, the next start automatically re-runs the row import (idempotent upserts — no duplicates) or resumes the FTS rebuild where it stopped. TIGER counties already imported are skipped individually.
- **Bounded disk usage.** TIGER ZIPs and shapefiles are deleted as each county finishes (`TIGER_KEEP_DATA=false`), so the TIGER phase needs only one county's worth of scratch space instead of ~100GB. Set `OSM_KEEP_DATA=false` to also delete the OSM extract after import.
- **Bounded memory.** A `GOMEMLIMIT`-style heap cap (default 1500MiB) makes the Go GC reclaim memory before the kernel OOM killer fires. The SQLite driver is pure Go, so its cache is covered too.
- **Atomic, retried downloads.** Downloads land in a `.part` file and are renamed on success, with 3 attempts — a killed download is never mistaken for a cached file.
- A `wal_checkpoint(TRUNCATE)` + `PRAGMA optimize` runs after each bulk phase to reclaim disk and refresh query statistics.

## Data Storage

Geocoder data is stored in PocketBase's main SQLite database (`pb_data/data.db`):

- `geocoder_places` — a **PocketBase collection** containing the indexed place records. This collection is visible and manageable from the PocketBase admin UI at `/_/collections/geocoder_places`.
- `geocoder_places_fts` — an FTS5 virtual table for full-text address search. This is a raw SQL table (not a collection) because FTS5 virtual tables are not directly supported as PocketBase collection types. It is kept in sync with `geocoder_places` via SQL triggers.
- `tiger_addr_ranges` — TIGER/Line address range data for address interpolation (see below).
- `tiger_addr_fts` — FTS5 index over `tiger_addr_ranges` for fast street name prefix matching.
- `zip_city_state` — a cache table mapping ZIP codes to their most common city/state (derived from OSM data). Used to enrich TIGER results with city/state names via a JOIN, avoiding N+1 per-row lookups against the 54M-row `geocoder_places` table.

The schema is created automatically on first startup by `internal/db/migrations.go`.

## Initial Data Fetch

On first startup, the app checks whether `geocoder_places` is empty. If it is, it automatically downloads the configured OSM extract and indexes it. The import runs in the background so the server starts immediately.

### Recovering from Interrupted Imports

Recovery is automatic. The app records import progress in the `_import_state` table, so if the process is killed mid-import, the next startup detects the incomplete state and resumes: the row import is re-run with idempotent upserts (no duplicates), an interrupted FTS index rebuild continues from its last committed chunk, and TIGER counties that were already imported are skipped.

To force a clean full re-import anyway (e.g. after a schema change):

```bash
FORCE_REINDEX=true ./geocoder-pb serve
```

This re-imports all OSM data from the downloaded PBF file (existing rows are updated in place via upserts).

### Import Performance

The importer uses batched transactions with multi-row INSERT statements (2,000 records per batch by default, configurable via `IMPORT_BATCH_SIZE`) for dramatically faster inserts compared to one-by-one inserts. Progress is logged every 50,000 records with count, elapsed time, and records/second throughput.

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
```

### API Endpoints

All endpoints are under `/api/geocoder/` and return JSON with a `results` array of place objects.

#### `GET /api/geocoder/search`

Full-text search for places and addresses.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `q` | string | *(required)* | Search query (e.g., `1600 Pennsylvania Avenue NW, Washington, DC`) |
| `limit` | int | `10` | Maximum number of results |
| `bbox` | string | *(empty)* | Bounding box filter in `minLng,minLat,maxLng,maxLat` format (e.g., `-73.5,41.3,-72.8,41.6`). When provided, only results within the box are returned. |

**Example:**

```bash
curl "http://127.0.0.1:8090/api/geocoder/search?q=Calico&limit=5"
curl "http://127.0.0.1:8090/api/geocoder/search?q=Calico&bbox=-73.5,41.3,-72.8,41.6"
```

```json
{
  "query": "Calico",
  "results": [
    {
      "id": "way/1077359346",
      "osm_id": 1077359346,
      "osm_type": "way",
      "name": "Calico Drive",
      "address": "Calico Drive",
      "city": "",
      "state": "",
      "postcode": "",
      "country": "",
      "lat": 0,
      "lon": 0,
      "class": "",
      "type": "",
      "importance": 0,
      "created": "2026-07-02 04:26:37",
      "updated": "2026-07-02 04:26:37"
    }
  ]
}
```

#### `GET /api/geocoder/autocomplete`

Prefix-based search optimized for type-ahead autocomplete. Each whitespace-separated token is treated as a prefix (e.g., `12 Eng` matches `12 Englewood Avenue`, `126 Englehutt Road`).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `q` | string | *(required)* | Partial query (e.g., `12 Englewood Ave`) |
| `limit` | int | `10` | Maximum number of results |
| `bbox` | string | *(empty)* | Bounding box filter in `minLng,minLat,maxLng,maxLat` format. When provided, only results within the box are returned. |

**Example:**

```bash
curl "http://127.0.0.1:8090/api/geocoder/autocomplete?q=12+Englewood+Ave&limit=5"
curl "http://127.0.0.1:8090/api/geocoder/autocomplete?q=12+Englewood+Ave&bbox=-73.5,41.3,-72.8,41.6"
```

```json
{
  "query": "12 Englewood Ave",
  "results": [
    {
      "id": "way/214216828",
      "osm_id": 214216828,
      "osm_type": "way",
      "name": "12 Englewood Avenue",
      "address": "12 Englewood Avenue",
      "city": "",
      "state": "",
      "postcode": "",
      "country": "",
      "lat": 0,
      "lon": 0,
      "class": "",
      "type": "",
      "importance": 0,
      "created": "2026-07-02 04:33:28",
      "updated": "2026-07-02 04:33:28"
    },
    {
      "id": "node/8713212605",
      "osm_id": 8713212605,
      "osm_type": "node",
      "name": "122 Englewood Avenue",
      "address": "122 Englewood Avenue",
      "city": "Prospect",
      "state": "CT",
      "postcode": "06712",
      "country": "",
      "lat": 41.479864,
      "lon": -72.955738,
      "class": "",
      "type": "",
      "importance": 0,
      "created": "2026-07-02 04:04:24",
      "updated": "2026-07-02 04:04:24"
    }
  ]
}
```

#### `GET /api/geocoder/reverse`

Reverse geocoding — find the nearest place to given coordinates.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `lat` | float | *(required)* | Latitude |
| `lon` | float | *(required)* | Longitude |
| `limit` | int | `1` | Maximum number of results |

**Example:**

```bash
curl "http://127.0.0.1:8090/api/geocoder/reverse?lat=38.8977&lon=-77.0365"
```

### Response Object Fields

Each result in the `results` array contains:

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Composite ID: `{osm_type}/{osm_id}` (e.g., `way/214216828`) |
| `osm_id` | int64 | OpenStreetMap element ID |
| `osm_type` | string | `node`, `way`, or `relation` |
| `name` | string | Place name or full address line |
| `address` | string | Full address string |
| `city` | string | City name (populated for address nodes) |
| `state` | string | State abbreviation (populated for address nodes) |
| `postcode` | string | Postal code (populated for address nodes) |
| `country` | string | Country name |
| `lat` | float | Latitude (0 for ways without resolved coordinates) |
| `lon` | float | Longitude (0 for ways without resolved coordinates) |
| `class` | string | OSM class category |
| `type` | string | OSM type within class |
| `importance` | float | Search importance ranking |
| `created` | string | Record creation timestamp |
| `updated` | string | Record last-update timestamp |

## Comparison with Komoot Photon

[Photon](https://photon.komoot.io/) is a popular open-source geocoder built on Elasticsearch with OSM data. `geocoder-pb` was designed as a lightweight, self-hosted alternative to Photon, but it is **not a drop-in replacement** — the API response format differs.

### Key Differences

| Feature | Photon | geocoder-pb |
|---------|--------|-------------|
| **Response format** | GeoJSON `FeatureCollection` | Simple JSON `{ query, results: [...] }` |
| **Coordinates** | `geometry.coordinates: [lon, lat]` (GeoJSON order) | Separate `lat` and `lon` fields |
| **OSM type** | Single letter: `N`, `W`, `R` | Full word: `node`, `way`, `relation` |
| **Tag fields** | `osm_key`, `osm_value` | `class`, `type` |
| **Address fields** | Separate `housenumber`, `street`, `city`, `state`, `postcode`, `country` | Combined `address` string + separate `city`, `state`, `postcode`, `country` |
| **Location bias** | `lat`/`lon` params in search to bias results | Not supported (use `bbox` filter instead) |
| **Bbox filtering** | `bbox=minLng,minLat,maxLng,maxLat` | `bbox=minLng,minLat,maxLng,maxLat` |
| **Language** | `lang` param (e.g., `de`, `en`) | Not supported |
| **Tag filtering** | `osm_tag=highway:residential` | Not supported |
| **Backend** | Elasticsearch (Java, ~2GB RAM) | SQLite FTS5 (Go, <100MB RAM) |
| **Deployment** | Docker + Elasticsearch | Single binary |

### Photon Response Example

```json
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "geometry": {
        "type": "Point",
        "coordinates": [-72.955738, 41.479864]
      },
      "properties": {
        "osm_id": 8713212605,
        "osm_type": "N",
        "osm_key": "place",
        "osm_value": "house",
        "name": "122 Englewood Avenue",
        "housenumber": "122",
        "street": "Englewood Avenue",
        "city": "Prospect",
        "state": "Connecticut",
        "postcode": "06712",
        "country": "United States"
      }
    }
  ]
}
```

### Migrating from Photon

If you're replacing a Photon deployment, you'll need to adapt your client code to handle the different response format. A minimal adapter could map:

- `features[].geometry.coordinates[1]` → `results[].lat`
- `features[].geometry.coordinates[0]` → `results[].lon`
- `features[].properties.osm_type` `"N"`/`"W"`/`"R"` → `results[].osm_type` `"node"`/`"way"`/`"relation"`
- `features[].properties.osm_key`/`osm_value` → `results[].class`/`type`

## TIGER/Line Address Interpolation

In addition to OSM data, `geocoder-pb` imports [TIGER/Line](https://www.census.gov/geographies/mapping-files/time-series/geo/tiger-line-file.html) ADDRFEAT address range data from the US Census Bureau. This fills coverage gaps for addresses that exist in TIGER but not in OSM (e.g., house-numbered streets without address nodes in OSM).

When a search or autocomplete query starts with a house number (e.g., `42 Maple Street`), the geocoder looks up matching TIGER address ranges and interpolates the coordinates. City and state names are enriched from OSM data using the ZIP code, since TIGER ADDRFEAT only contains ZIP codes.

### Configuration

By default, TIGER/Line data is imported for **all US counties** (~3,200 counties). This provides nationwide address coverage but may take several hours on first import. Source files are deleted as each county completes (unless `TIGER_KEEP_DATA=true`), so the import needs little scratch disk space.

To limit the import to specific counties, set `TIGER_ALL_COUNTIES=false` and specify county FIPS codes:

```bash
# Import only Middlesex County, MA (Cambridge area)
TIGER_ALL_COUNTIES=false TIGER_COUNTIES=25017 ./geocoder-pb serve
```

To force a re-import of TIGER data (e.g., after upgrading to a new year):

```bash
TIGER_FORCE_REIMPORT=1 ./geocoder-pb serve
```

### Verifying the Import

Check the number of imported address ranges:

```bash
sqlite3 pb_data/data.db "SELECT COUNT(*) FROM tiger_addr_ranges;"
```

A complete US import should have ~30M+ rows. You can spot-check specific addresses:

```bash
sqlite3 pb_data/data.db "SELECT * FROM tiger_addr_ranges WHERE full_name = 'Birchwood Rd' AND zip = '88316';"
```

## Data Refresh

Set `UPDATE_CRON` to a valid cron expression to automatically download and re-index OSM data on a schedule. For example:

```bash
UPDATE_CRON="0 3 * * 0" ./geocoder-pb serve
```

## License

MIT