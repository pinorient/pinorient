package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/pinorient/pinorient/internal/config"
	"github.com/pinorient/pinorient/internal/geocoder"
)

// RegisterRoutes wires up the geocoder API routes and domain restriction middleware.
func RegisterRoutes(e *core.ServeEvent, geo *geocoder.Geocoder, cfg *config.Config) {
	g := e.Router.Group("/api/geocoder")
	g.BindFunc(domainRestrictionMiddleware(cfg.AllowedDomains))
	g.GET("/search", searchHandler(geo))
	g.GET("/autocomplete", autocompleteHandler(geo))
	g.GET("/reverse", reverseHandler(geo))
}

func searchHandler(geo *geocoder.Geocoder) func(e *core.RequestEvent) error {
	return func(c *core.RequestEvent) error {
		q := strings.TrimSpace(c.Request.URL.Query().Get("q"))
		if q == "" {
			return c.BadRequestError("missing query parameter 'q'", nil)
		}

		limit, _ := strconv.Atoi(c.Request.URL.Query().Get("limit"))
		bbox := parseBBox(c.Request.URL.Query().Get("bbox"))

		results, err := geo.Search(c.Request.Context(), q, limit, bbox)
		if err != nil {
			return c.InternalServerError("search failed", err)
		}

		return c.JSON(http.StatusOK, map[string]any{
			"query":   q,
			"results": results,
		})
	}
}

func autocompleteHandler(geo *geocoder.Geocoder) func(e *core.RequestEvent) error {
	return func(c *core.RequestEvent) error {
		q := strings.TrimSpace(c.Request.URL.Query().Get("q"))
		if q == "" {
			return c.JSON(http.StatusOK, map[string]any{
				"query":   q,
				"results": []any{},
			})
		}

		limit, _ := strconv.Atoi(c.Request.URL.Query().Get("limit"))
		bbox := parseBBox(c.Request.URL.Query().Get("bbox"))

		results, err := geo.Autocomplete(c.Request.Context(), q, limit, bbox)
		if err != nil {
			return c.InternalServerError("autocomplete failed", err)
		}

		return c.JSON(http.StatusOK, map[string]any{
			"query":   q,
			"results": results,
		})
	}
}

// parseBBox parses a bounding box string in "minLng,minLat,maxLng,maxLat" format
// (matching the Photon API convention). Returns nil if the string is empty or invalid.
func parseBBox(s string) *geocoder.BBox {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return nil
	}

	minLng, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return nil
	}
	minLat, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return nil
	}
	maxLng, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return nil
	}
	maxLat, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return nil
	}

	return &geocoder.BBox{
		MinLng: minLng,
		MinLat: minLat,
		MaxLng: maxLng,
		MaxLat: maxLat,
	}
}

func reverseHandler(geo *geocoder.Geocoder) func(e *core.RequestEvent) error {
	return func(c *core.RequestEvent) error {
		lat, err := strconv.ParseFloat(c.Request.URL.Query().Get("lat"), 64)
		if err != nil {
			return c.BadRequestError("invalid lat parameter", err)
		}

		lon, err := strconv.ParseFloat(c.Request.URL.Query().Get("lon"), 64)
		if err != nil {
			return c.BadRequestError("invalid lon parameter", err)
		}

		limit, _ := strconv.Atoi(c.Request.URL.Query().Get("limit"))
		results, err := geo.Reverse(c.Request.Context(), lat, lon, limit)
		if err != nil {
			return c.InternalServerError("reverse geocoding failed", err)
		}

		return c.JSON(http.StatusOK, map[string]any{
			"lat":     lat,
			"lon":     lon,
			"results": results,
		})
	}
}

func domainRestrictionMiddleware(allowed []string) func(e *core.RequestEvent) error {
	return func(c *core.RequestEvent) error {
		if len(allowed) == 0 {
			return c.Next()
		}

		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			origin = c.Request.Header.Get("Referer")
		}

		if !isDomainAllowed(origin, allowed) {
			return c.ForbiddenError("domain not allowed", nil)
		}

		return c.Next()
	}
}

func isDomainAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return false
	}

	// Strip scheme and path, keep only host.
	origin = strings.TrimPrefix(origin, "http://")
	origin = strings.TrimPrefix(origin, "https://")
	if idx := strings.Index(origin, "/"); idx != -1 {
		origin = origin[:idx]
	}
	if idx := strings.Index(origin, ":"); idx != -1 {
		origin = origin[:idx]
	}

	for _, d := range allowed {
		if strings.HasPrefix(d, "*.") {
			suffix := d[1:] // .mysite.com
			if strings.HasSuffix(origin, suffix) {
				return true
			}
		}
		if origin == d {
			return true
		}
	}

	return false
}

// unusedRouterImport prevents goimports from removing the router import.
var _ = router.NewApiError
