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
```

### API Endpoints

All endpoints are under `/api/geocoder/` and return JSON with a `results` array of place objects.

#### `GET /api/geocoder/search`

Full-text search for places and addresses.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `q` | string | *(required)* | Search query (e.g., `1600 Pennsylvania Avenue NW, Washington, DC`) |
| `limit` | int | `10` | Maximum number of results |

**Example:**

```bash
curl "http://127.0.0.1:8090/api/geocoder/search?q=Calico&limit=5"
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

**Example:**

```bash
curl "http://127.0.0.1:8090/api/geocoder/autocomplete?q=12+Englewood+Ave&limit=5"
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
| **Location bias** | `lat`/`lon` params in search to bias results | Not supported |
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

## Data Refresh

Set `UPDATE_CRON` to a valid cron expression to automatically download and re-index OSM data on a schedule. For example:

```bash
UPDATE_CRON="0 3 * * 0" ./geocoder-pb serve
```

## License

MIT