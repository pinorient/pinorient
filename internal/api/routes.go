package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/pinorient/pinorient/internal/geocoder"
)

// DomainCheckerFunc decides whether a request origin is allowed to use the
// geocoder API. It receives the raw Origin (or Referer) header value and
// returns true if the request may proceed.
type DomainCheckerFunc func(origin string) bool

// EnvDomainChecker returns the default domain checker, matching the
// historical ALLOWED_DOMAINS behavior: an empty allowed list permits all
// origins, otherwise the origin host must match one of the entries exactly
// or via a wildcard prefix like *.mysite.com.
func EnvDomainChecker(allowed []string) DomainCheckerFunc {
	return func(origin string) bool {
		if len(allowed) == 0 {
			return true
		}
		return isDomainAllowed(origin, allowed)
	}
}

// RegisterRoutes wires up the geocoder API routes and domain restriction
// middleware. Additional middleware (e.g. rate limiting, usage metering)
// can be supplied via extra and is bound after the domain check.
func RegisterRoutes(e *core.ServeEvent, geo *geocoder.Geocoder, checker DomainCheckerFunc, extra ...func(*core.RequestEvent) error) {
	g := e.Router.Group("/api/geocoder")
	g.BindFunc(domainRestrictionMiddleware(checker))
	if len(extra) > 0 {
		g.BindFunc(extra...)
	}
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

func domainRestrictionMiddleware(checker DomainCheckerFunc) func(e *core.RequestEvent) error {
	return func(c *core.RequestEvent) error {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			origin = c.Request.Header.Get("Referer")
		}

		if !checker(origin) {
			return c.ForbiddenError("domain not allowed", nil)
		}

		return c.Next()
	}
}

// NormalizeOrigin strips the scheme, path, and port from an Origin or
// Referer header value, returning only the host. Empty input returns "".
func NormalizeOrigin(origin string) string {
	if origin == "" {
		return ""
	}
	origin = strings.TrimPrefix(origin, "http://")
	origin = strings.TrimPrefix(origin, "https://")
	if idx := strings.Index(origin, "/"); idx != -1 {
		origin = origin[:idx]
	}
	if idx := strings.Index(origin, ":"); idx != -1 {
		origin = origin[:idx]
	}
	return origin
}

func isDomainAllowed(origin string, allowed []string) bool {
	origin = NormalizeOrigin(origin)
	if origin == "" {
		return false
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

