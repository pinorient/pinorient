Overview: A pure Golang / Sqlite Geocoder for translating written addresses into map coordinates using OpenStreetMap (OSM)

Technology:
- PocketBase (v0.39.0 or newer) extended as a Go framework
- Sqlite with FTS5
- A OSM parsing library (like qedus/osmpbf)

Problem we're solving:
I have multiple PocketBase instances that were using the https://photon.komoot.io/ API, but were getting rate limited because of too many requests. I looked into hosting my own Photon instance, but decided it was too much overhead to manage Docker containers with Java/ElasticSearch/NGinx/etc.

Regions:
We only need OSM data for the US for now

Keeping data up-to-date:
I'd like to periodically retrieve new OSM data so newly added address can be found.

Restrictions:
I'd like to be able to restrict access to only certain domains, like *.mysite.com
