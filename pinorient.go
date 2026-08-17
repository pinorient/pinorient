// Package pinorient provides the public, embeddable entrypoint for the
// pinorient geocoder. Downstream applications create a PocketBase instance
// with New, wire the geocoder into it with Setup, and start serving.
//
//	app := pinorient.New()
//	if err := pinorient.Setup(app); err != nil {
//		log.Fatal(err)
//	}
//	app.Start()
package pinorient

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"runtime/debug"
	"strconv"

	"github.com/joho/godotenv"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/pinorient/pinorient/internal/api"
	"github.com/pinorient/pinorient/internal/config"
)

// Config holds application configuration. It is an alias of the internal
// config type so downstream applications can construct or tweak it directly.
type Config = config.Config

// DomainCheckerFunc decides whether a request origin is allowed to use the
// geocoder API.
type DomainCheckerFunc = api.DomainCheckerFunc

// EnvDomainChecker returns the default ALLOWED_DOMAINS-based domain checker.
var EnvDomainChecker = api.EnvDomainChecker

// NormalizeOrigin strips scheme, path, and port from an Origin/Referer header.
var NormalizeOrigin = api.NormalizeOrigin

// LoadConfig reads configuration from environment variables.
func LoadConfig() *Config { return config.Load() }

// options holds optional Setup configuration.
type options struct {
	cfg            *Config
	domainChecker  DomainCheckerFunc
	middleware     []func(*core.RequestEvent) error
	homepage       fs.FS
	homepageEnable bool
}

// Option customizes the behavior of Setup.
type Option func(*options)

// WithConfig uses an explicitly constructed Config instead of loading one
// from environment variables.
func WithConfig(cfg *Config) Option {
	return func(o *options) { o.cfg = cfg }
}

// WithDomainChecker overrides the default ALLOWED_DOMAINS-based domain
// restriction with custom logic (e.g. per-user domain whitelists).
func WithDomainChecker(checker DomainCheckerFunc) Option {
	return func(o *options) { o.domainChecker = checker }
}

// WithMiddleware binds additional middleware to the /api/geocoder route
// group, after the domain check (e.g. rate limiting or usage metering).
func WithMiddleware(mw ...func(*core.RequestEvent) error) Option {
	return func(o *options) { o.middleware = append(o.middleware, mw...) }
}

// WithHomepage serves the given filesystem (which must contain index.html)
// at / instead of the built-in default homepage.
func WithHomepage(fsys fs.FS) Option {
	return func(o *options) {
		o.homepage = fsys
		o.homepageEnable = true
	}
}

// WithoutHomepage disables serving the static homepage at / entirely.
func WithoutHomepage() Option {
	return func(o *options) {
		o.homepage = nil
		o.homepageEnable = false
	}
}

// New creates a PocketBase instance with SQLite pragmas optimized for the
// geocoder workload. It also loads an optional .env file and applies the
// GOMEMLIMIT / GO_MEM_LIMIT_MB heap cap.
//
// Cache size and mmap size are configurable via environment variables to
// support servers with different amounts of RAM. Defaults: cache_size=64MB,
// mmap_size=0 (disabled) — safe for 2GB RAM servers. For machines with 8GB+
// RAM, set DB_CACHE_SIZE=262144 and DB_MMAP_SIZE=4294967296.
func New() *pocketbase.PocketBase {
	// Load .env file if it exists. Existing environment variables take
	// precedence over .env values.
	_ = godotenv.Load()

	// Cap the Go heap so the garbage collector works harder before the
	// kernel OOM killer gets involved. This matters on small (2GB RAM)
	// servers. The SQLite driver (modernc.org/sqlite) is pure Go, so its
	// page cache is covered by this limit too. GOMEMLIMIT always wins
	// when set; otherwise GO_MEM_LIMIT_MB applies, defaulting to 1500MiB.
	if os.Getenv("GOMEMLIMIT") == "" {
		limitMB := getEnvInt64("GO_MEM_LIMIT_MB", 1500)
		debug.SetMemoryLimit(limitMB << 20)
		log.Printf("go memory limit set to %dMiB (override with GOMEMLIMIT or GO_MEM_LIMIT_MB)", limitMB)
	}

	cacheSize := getEnvInt("DB_CACHE_SIZE", 65536) // in KB, 65536 = 64MB
	mmapSize := getEnvInt64("DB_MMAP_SIZE", 0)     // in bytes, 0 = disabled
	return pocketbase.NewWithConfig(pocketbase.Config{
		DBConnect: func(dbPath string) (*dbx.DB, error) {
			pragmas := fmt.Sprintf("?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(200000000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-%d)&_pragma=mmap_size(%d)", cacheSize, mmapSize)
			return dbx.Open("sqlite", dbPath+pragmas)
		},
	})
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
