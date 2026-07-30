// Package config turns the process environment into a validated, immutable
// configuration value.
//
// Every knob Encore exposes is an environment variable prefixed ENCORE_, and the
// full set is documented in docs/configuration.md and .env.example. Parsing
// collects *all* problems before returning, so a misconfigured deployment reports
// everything that is wrong in one go instead of one variable per restart.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of Encore's static configuration.
type Config struct {
	Env      string // "production" | "development"
	Instance Instance
	HTTP     HTTP
	Database Database
	Log      Log
	Security Security
	Spotify  Spotify
	Sync     Sync
	Import   Import
	Enrich   Enrich
	Metrics  Metrics
	Worker   Worker
	// MetadataFallback is an optional second source of catalogue metadata.
	MetadataFallback MetadataFallback
}

// Instance describes how the deployment presents itself to the outside world.
type Instance struct {
	// PublicURL is the externally reachable base URL of the API, used to build the
	// OAuth redirect URI. It must match what is registered in the Spotify dashboard.
	PublicURL string
	// WebURL is where browsers reach the frontend; OAuth journeys end there.
	WebURL string
	// DefaultTimezone seeds new users' statistics timezone.
	DefaultTimezone string
	// RegistrationsDefault seeds app_settings on a brand-new database only.
	RegistrationsDefault bool
}

type HTTP struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// MaxRequestBytes caps non-upload request bodies.
	MaxRequestBytes int64
	// CORSOrigins is the exact allow-list; empty means same-origin only, which is
	// the correct setting when the frontend is served by the bundled nginx.
	CORSOrigins []string
	// TrustProxyHeaders makes Encore believe X-Forwarded-For when recording the
	// address on a session. Only turn it on behind a reverse proxy you control,
	// because any client can otherwise set that header to anything.
	//
	// Encore deliberately does not derive URLs or cookie security from proxy
	// headers: those come from PublicURL and CookieSecure, so a misconfigured
	// proxy cannot downgrade a cookie or forge a redirect.
	TrustProxyHeaders bool
	// FrameAncestors populates the CSP frame-ancestors directive.
	FrameAncestors []string
}

type Database struct {
	URL string
	// MaxConns bounds the pool. The importer's backpressure story depends on this
	// being finite: when every connection is busy the reader blocks.
	MaxConns int32
	MinConns int32
	// ConnectTimeout applies to establishing a connection, not to queries.
	ConnectTimeout time.Duration
	// StatementTimeout is applied per session; it stops a runaway statistics query
	// from pinning a connection forever.
	StatementTimeout time.Duration
	// MigrateOnStart runs pending migrations during API startup. Off by default:
	// migrations are a deliberate, separately observable step.
	MigrateOnStart bool
}

type Log struct {
	Level  string // debug | info | warn | error
	Format string // json | text
	// Source adds file:line to every record. Useful in development, noisy in production.
	Source bool
}

type Security struct {
	// EncryptionKey is 32 raw bytes used for AES-256-GCM sealing of Spotify tokens
	// and PKCE verifiers at rest.
	EncryptionKey  []byte
	SessionTTL     time.Duration
	CookieName     string
	CookieDomain   string
	CookiePath     string
	CookieSecure   bool
	CookieSameSite string // lax | strict | none
}

type Spotify struct {
	ClientID     string
	ClientSecret string
	// RedirectURL defaults to PublicURL + /api/auth/spotify/callback.
	RedirectURL string
	Scopes      []string
	APIBaseURL  string
	AuthBaseURL string
	TokenURL    string
	// RateLimit is the sustained request rate Encore allows itself against the
	// Spotify API, shared by every worker in the process.
	//
	// The default is deliberately low. A Spotify application starts in
	// development mode, whose daily quota is small and undocumented, and
	// exceeding it does not earn a short pause — it earns a 429 carrying a
	// Retry-After of most of a day and a body saying QUOTA_EXCEEDED. Nothing
	// Encore does benefits from going faster: enriching a sixteen-thousand-track
	// backlog takes about three hundred requests, which at two per second is a
	// few minutes. Speed here buys nothing and risks a day of blank metadata.
	RateLimit float64
	RateBurst int
	// Timeout is the per-request HTTP timeout.
	Timeout time.Duration
	// MaxRetries bounds retries of a single Spotify request.
	MaxRetries int
}

type Sync struct {
	Enabled  bool
	Interval time.Duration
	// Concurrency is how many accounts are polled at once.
	Concurrency int
	// InitialLookback bounds the first poll for a newly connected account.
	InitialLookback time.Duration
}

type Import struct {
	// Dir is where uploaded exports are spooled. It must be durable storage
	// shared between the API and the worker, and it must survive restarts, since
	// a resumed import re-reads the original file.
	Dir string
	// BatchSize is the number of records accumulated before a flush. It is the
	// main lever on peak memory: memory is O(BatchSize x record size).
	BatchSize int
	// MaxUploadBytes caps a single upload.
	MaxUploadBytes int64
	// MinMsPlayed drops plays shorter than this as noise. They are counted as
	// skipped, not rejected. Set to 30000 to match Spotify's own definition of a
	// stream, or 0 to keep every event.
	MinMsPlayed int32
	// MaxRejectsPerFile bounds the diagnostics stored for one pathological export.
	MaxRejectsPerFile int
	// Workers is how many jobs one worker process runs concurrently.
	Workers int
	// LeaseTTL is how long a claim survives without a heartbeat. A crashed worker's
	// job becomes claimable this long after it dies.
	LeaseTTL time.Duration
	// BatchRetries bounds retries of a single failing batch before the job fails.
	BatchRetries int
	// RetainFiles keeps uploaded exports after a job completes so it can be
	// re-run. When false the file is deleted once the job is verified.
	RetainFiles bool
}

type Enrich struct {
	Enabled bool
	// Interval is how often an idle enrichment worker looks for new work.
	Interval time.Duration
	// BatchSize is bounded by Spotify: 50 tracks, 50 artists, 20 albums.
	BatchSize int
	// AliasEnabled turns on /v1/search resolution of names-only listens. It costs
	// one request per distinct (artist, title) pair, so it is rate limited hard.
	AliasEnabled bool
	// AliasRate is requests per second dedicated to alias resolution.
	AliasRate float64
	// RepairInterval is how often permanently failed catalogue rows are revisited.
	RepairInterval time.Duration
	// RollupInterval is how often dirty statistics rollup days are recomputed.
	RollupInterval time.Duration
}

// MetadataFallback configures a second source of catalogue metadata, consulted
// when Spotify is rate limiting the instance and for ids Spotify will not serve
// at all.
//
// It is off unless URL is set. Encore ships no fallback and endorses no
// particular one: the contract is three Spotify-shaped endpoints, documented in
// docs/metadata-fallback.md, and what an operator serves from theirs is their
// own business.
type MetadataFallback struct {
	// URL is the base of a Spotify-shaped API, without a trailing slash — the
	// part before /v1/tracks. Empty disables the whole feature.
	URL string
	// Token, when set, travels as `Authorization: Bearer <token>`.
	Token string
	// Timeout is the per-request HTTP timeout.
	Timeout time.Duration
	// BatchSize caps ids per request, never above Spotify's own limit of 50.
	BatchSize int
	// RateLimit is requests per second. Zero means unlimited, which is the
	// sensible default for a server the operator runs themselves.
	RateLimit float64
	RateBurst int
	// Prefer asks the fallback before Spotify, keeping Spotify for what the
	// fallback does not have.
	//
	// Defaults to true, because it is what somebody who went to the trouble of
	// running a metadata source wanted from it: the Spotify quota is then spent
	// only on ids the source lacks, and a development-mode application stops
	// exhausting it during the first import. The cost is freshness — a mirror is
	// a point-in-time copy — so it can be turned off for an instance that would
	// rather have current data and wait.
	Prefer bool
}

// Enabled reports whether a fallback has been configured.
func (m MetadataFallback) Enabled() bool { return strings.TrimSpace(m.URL) != "" }

type Metrics struct {
	Enabled bool
	// Username and Password enable basic auth on /metrics. Leave empty to expose
	// it unauthenticated, which is only sensible on a private network.
	Username string
	Password string
}

type Worker struct {
	// ID identifies this process in import job leases. Defaults to the hostname.
	ID string
}

// Load reads configuration from the process environment.
func Load() (*Config, error) { return parse(os.LookupEnv) }

// LoadFrom reads configuration from an explicit map. Used by tests.
func LoadFrom(env map[string]string) (*Config, error) {
	return parse(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
}

type lookup func(string) (string, bool)

func parse(get lookup) (*Config, error) {
	p := &parser{get: get}
	c := &Config{}

	c.Env = p.enum("ENCORE_ENV", "production", "production", "development")
	dev := c.Env == "development"

	c.Instance = Instance{
		PublicURL:            strings.TrimRight(p.requiredURL("ENCORE_PUBLIC_URL"), "/"),
		WebURL:               strings.TrimRight(p.requiredURL("ENCORE_WEB_URL"), "/"),
		DefaultTimezone:      p.timezone("ENCORE_DEFAULT_TIMEZONE", "UTC"),
		RegistrationsDefault: p.boolean("ENCORE_REGISTRATIONS_DEFAULT", true),
	}

	c.HTTP = HTTP{
		Addr:              p.str("ENCORE_HTTP_ADDR", ":8080"),
		ReadTimeout:       p.duration("ENCORE_HTTP_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:      p.duration("ENCORE_HTTP_WRITE_TIMEOUT", 60*time.Second),
		IdleTimeout:       p.duration("ENCORE_HTTP_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout:   p.duration("ENCORE_HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		MaxRequestBytes:   p.bytes("ENCORE_HTTP_MAX_REQUEST_BYTES", 1<<20),
		CORSOrigins:       p.list("ENCORE_CORS_ORIGINS"),
		TrustProxyHeaders: p.boolean("ENCORE_TRUST_PROXY_HEADERS", false),
		FrameAncestors:    p.list("ENCORE_FRAME_ANCESTORS"),
	}

	c.Database = Database{
		URL:              p.required("ENCORE_DATABASE_URL"),
		MaxConns:         int32(p.intRange("ENCORE_DATABASE_MAX_CONNS", 10, 1, 1000)),
		MinConns:         int32(p.intRange("ENCORE_DATABASE_MIN_CONNS", 0, 0, 1000)),
		ConnectTimeout:   p.duration("ENCORE_DATABASE_CONNECT_TIMEOUT", 10*time.Second),
		StatementTimeout: p.duration("ENCORE_DATABASE_STATEMENT_TIMEOUT", 60*time.Second),
		MigrateOnStart:   p.boolean("ENCORE_DATABASE_MIGRATE_ON_START", false),
	}
	if c.Database.MinConns > c.Database.MaxConns {
		p.errf("ENCORE_DATABASE_MIN_CONNS (%d) must not exceed ENCORE_DATABASE_MAX_CONNS (%d)",
			c.Database.MinConns, c.Database.MaxConns)
	}

	c.Log = Log{
		Level:  p.enum("ENCORE_LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		Format: p.enum("ENCORE_LOG_FORMAT", defaultIf(dev, "text", "json"), "json", "text"),
		Source: p.boolean("ENCORE_LOG_SOURCE", dev),
	}

	c.Security = Security{
		EncryptionKey:  p.key("ENCORE_ENCRYPTION_KEY"),
		SessionTTL:     p.duration("ENCORE_SESSION_TTL", 30*24*time.Hour),
		CookieName:     p.str("ENCORE_COOKIE_NAME", "encore_session"),
		CookieDomain:   p.str("ENCORE_COOKIE_DOMAIN", ""),
		CookiePath:     p.str("ENCORE_COOKIE_PATH", "/"),
		CookieSecure:   p.boolean("ENCORE_COOKIE_SECURE", !dev),
		CookieSameSite: p.enum("ENCORE_COOKIE_SAMESITE", "lax", "lax", "strict", "none"),
	}
	if c.Security.CookieSameSite == "none" && !c.Security.CookieSecure {
		p.errf("ENCORE_COOKIE_SAMESITE=none requires ENCORE_COOKIE_SECURE=true; browsers reject the combination")
	}

	c.Spotify = Spotify{
		ClientID:     p.required("ENCORE_SPOTIFY_CLIENT_ID"),
		ClientSecret: p.required("ENCORE_SPOTIFY_CLIENT_SECRET"),
		RedirectURL:  p.str("ENCORE_SPOTIFY_REDIRECT_URL", ""),
		Scopes:       DefaultScopes(),
		APIBaseURL:   strings.TrimRight(p.str("ENCORE_SPOTIFY_API_BASE_URL", "https://api.spotify.com"), "/"),
		AuthBaseURL:  strings.TrimRight(p.str("ENCORE_SPOTIFY_AUTH_BASE_URL", "https://accounts.spotify.com"), "/"),
		RateLimit:    p.float("ENCORE_SPOTIFY_RATE_LIMIT", 2),
		RateBurst:    p.intRange("ENCORE_SPOTIFY_RATE_BURST", 4, 1, 10000),
		Timeout:      p.duration("ENCORE_SPOTIFY_TIMEOUT", 20*time.Second),
		MaxRetries:   p.intRange("ENCORE_SPOTIFY_MAX_RETRIES", 5, 0, 20),
	}
	c.Spotify.TokenURL = c.Spotify.AuthBaseURL + "/api/token"
	if c.Spotify.RedirectURL == "" && c.Instance.PublicURL != "" {
		c.Spotify.RedirectURL = c.Instance.PublicURL + "/api/auth/spotify/callback"
	}

	c.Sync = Sync{
		Enabled:         p.boolean("ENCORE_SYNC_ENABLED", true),
		Interval:        p.duration("ENCORE_SYNC_INTERVAL", 60*time.Second),
		Concurrency:     p.intRange("ENCORE_SYNC_CONCURRENCY", 4, 1, 256),
		InitialLookback: p.duration("ENCORE_SYNC_INITIAL_LOOKBACK", 14*24*time.Hour),
	}

	c.Import = Import{
		Dir:               p.str("ENCORE_IMPORT_DIR", "/var/lib/encore/imports"),
		BatchSize:         p.intRange("ENCORE_IMPORT_BATCH_SIZE", 1000, 1, 100000),
		MaxUploadBytes:    p.bytes("ENCORE_IMPORT_MAX_UPLOAD_BYTES", 4<<30),
		MinMsPlayed:       int32(p.intRange("ENCORE_IMPORT_MIN_MS", 1000, 0, 24*60*60*1000)),
		MaxRejectsPerFile: p.intRange("ENCORE_IMPORT_MAX_REJECTS_PER_FILE", 1000, 0, 1000000),
		Workers:           p.intRange("ENCORE_IMPORT_WORKERS", 1, 1, 64),
		LeaseTTL:          p.duration("ENCORE_IMPORT_LEASE_TTL", 60*time.Second),
		BatchRetries:      p.intRange("ENCORE_IMPORT_BATCH_RETRIES", 6, 0, 30),
		RetainFiles:       p.boolean("ENCORE_IMPORT_RETAIN_FILES", true),
	}

	c.MetadataFallback = MetadataFallback{
		URL:       p.optionalURL("ENCORE_METADATA_FALLBACK_URL"),
		Token:     p.str("ENCORE_METADATA_FALLBACK_TOKEN", ""),
		Timeout:   p.duration("ENCORE_METADATA_FALLBACK_TIMEOUT", 10*time.Second),
		BatchSize: p.intRange("ENCORE_METADATA_FALLBACK_BATCH_SIZE", 50, 1, 50),
		RateLimit: p.float("ENCORE_METADATA_FALLBACK_RATE_LIMIT", 0),
		RateBurst: p.intRange("ENCORE_METADATA_FALLBACK_RATE_BURST", 1, 1, 1000),
		Prefer:    p.boolean("ENCORE_METADATA_FALLBACK_PREFER", true),
	}

	if c.MetadataFallback.Token != "" && !c.MetadataFallback.Enabled() {
		p.errf("ENCORE_METADATA_FALLBACK_TOKEN is set but ENCORE_METADATA_FALLBACK_URL is not, " +
			"so no fallback is configured")
	}

	c.Enrich = Enrich{
		Enabled:        p.boolean("ENCORE_ENRICH_ENABLED", true),
		Interval:       p.duration("ENCORE_ENRICH_INTERVAL", 5*time.Second),
		BatchSize:      p.intRange("ENCORE_ENRICH_BATCH_SIZE", 50, 1, 50),
		AliasEnabled:   p.boolean("ENCORE_ENRICH_ALIAS_ENABLED", true),
		AliasRate:      p.float("ENCORE_ENRICH_ALIAS_RATE", 2),
		RepairInterval: p.duration("ENCORE_ENRICH_REPAIR_INTERVAL", 6*time.Hour),
		RollupInterval: p.duration("ENCORE_ROLLUP_INTERVAL", 30*time.Second),
	}

	c.Metrics = Metrics{
		Enabled:  p.boolean("ENCORE_METRICS_ENABLED", true),
		Username: p.str("ENCORE_METRICS_USERNAME", ""),
		Password: p.str("ENCORE_METRICS_PASSWORD", ""),
	}
	if (c.Metrics.Username == "") != (c.Metrics.Password == "") {
		p.errf("ENCORE_METRICS_USERNAME and ENCORE_METRICS_PASSWORD must be set together")
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "encore-worker"
	}
	c.Worker = Worker{ID: p.str("ENCORE_WORKER_ID", host)}

	if err := p.err(); err != nil {
		return nil, err
	}
	return c, nil
}

// DefaultScopes is the grant Encore asks for at sign-in.
//
// Every one of these is read-only. Encore never asks, at sign-in, for
// permission to change anything about a listener's Spotify account: the two
// write scopes it can ever hold — playlist-modify-private and
// ugc-image-upload — are requested together at the moment somebody creates a
// playlist, and an account that never creates one is never asked.
//
// The read set is granted in one step rather than feature by feature. Five
// separate consent interruptions, each explaining a statistic the listener has
// not seen yet, is a worse experience than one; and every one of these is
// inert on its own — reading what somebody saved, follows, or ranked highly
// cannot alter any of it. See docs/security.md.
func DefaultScopes() []string {
	return []string{
		"user-read-recently-played",
		"user-read-private",
		"user-read-email",
		// Spotify's own ranking, to diff against Encore's.
		"user-top-read",
		// Saved tracks and albums, for saved-but-never-played.
		"user-library-read",
		// Followed artists, for followed-but-dormant.
		"user-follow-read",
		// Playlist names, so a listen's playlist context can be named.
		"playlist-read-private",
		// Device and shuffle state for the optional now-playing poller.
		"user-read-playback-state",
	}
}

// Redacted returns a copy safe to log: secrets are replaced, not merely shortened.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"env":                c.Env,
		"public_url":         c.Instance.PublicURL,
		"web_url":            c.Instance.WebURL,
		"http_addr":          c.HTTP.Addr,
		"database":           redactDSN(c.Database.URL),
		"database_max_conns": c.Database.MaxConns,
		"log_level":          c.Log.Level,
		"log_format":         c.Log.Format,
		"cookie_secure":      c.Security.CookieSecure,
		"cookie_samesite":    c.Security.CookieSameSite,
		"session_ttl":        c.Security.SessionTTL.String(),
		"spotify_client_id":  maskTail(c.Spotify.ClientID),
		"spotify_redirect":   c.Spotify.RedirectURL,
		"spotify_rate_limit": c.Spotify.RateLimit,
		"sync_enabled":       c.Sync.Enabled,
		"sync_interval":      c.Sync.Interval.String(),
		"import_dir":         c.Import.Dir,
		"import_batch_size":  c.Import.BatchSize,
		"import_workers":     c.Import.Workers,
		"import_min_ms":      c.Import.MinMsPlayed,
		"enrich_enabled":     c.Enrich.Enabled,
		// The URL is operational information worth having in a startup log; the
		// token is a credential and only its presence is reported.
		"metadata_fallback":      c.MetadataFallback.URL,
		"metadata_fallback_auth": c.MetadataFallback.Token != "",
		"metrics_enabled":        c.Metrics.Enabled,
		"metrics_auth":           c.Metrics.Username != "",
		"worker_id":              c.Worker.ID,
	}
}

// redactDSN keeps the shape of a connection string without its password.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "invalid"
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	q := u.Query()
	for _, k := range []string{"password", "sslpassword"} {
		if q.Has(k) {
			q.Set(k, "xxxxx")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// maskTail shows only enough of a public identifier to correlate it with the
// Spotify dashboard.
func maskTail(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("x", len(s))
	}
	return strings.Repeat("x", len(s)-4) + s[len(s)-4:]
}

func defaultIf[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// --- parsing helpers -------------------------------------------------------

type parser struct {
	get      lookup
	problems []string
}

func (p *parser) errf(format string, args ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, args...))
}

func (p *parser) err() error {
	if len(p.problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(p.problems, "\n  - "))
}

func (p *parser) raw(key string) (string, bool) {
	v, ok := p.get(key)
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, ok
}

func (p *parser) str(key, def string) string {
	if v, ok := p.raw(key); ok {
		return v
	}
	return def
}

func (p *parser) required(key string) string {
	v, ok := p.raw(key)
	if !ok {
		p.errf("%s is required", key)
	}
	return v
}

func (p *parser) requiredURL(key string) string {
	v := p.required(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		p.errf("%s must be an absolute URL such as https://encore.example.com, got %q", key, v)
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		p.errf("%s must use http or https, got %q", key, u.Scheme)
	}
	return v
}

// optionalURL validates an absolute http(s) URL when one is set, and returns ""
// when it is not. Used for the endpoints that switch a feature on by existing.
func (p *parser) optionalURL(key string) string {
	v := strings.TrimRight(strings.TrimSpace(p.str(key, "")), "/")
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		p.errf("%s must be an absolute URL such as https://metadata.example.com, got %q", key, v)
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		p.errf("%s must use http or https, got %q", key, u.Scheme)
	}
	return v
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.ToLower(v))
	if err != nil {
		p.errf("%s must be a boolean (true/false/1/0), got %q", key, v)
		return def
	}
	return b
}

func (p *parser) intRange(key string, def, min, max int) int {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.errf("%s must be an integer, got %q", key, v)
		return def
	}
	if n < min || n > max {
		p.errf("%s must be between %d and %d, got %d", key, min, max, n)
		return def
	}
	return n
}

func (p *parser) float(key string, def float64) float64 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		p.errf("%s must be a positive number, got %q", key, v)
		return def
	}
	return f
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	// A bare integer is accepted as seconds, which is what most people type.
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			p.errf("%s must be positive, got %q", key, v)
			return def
		}
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.errf("%s must be a positive duration such as 30s, 5m or 2h, got %q", key, v)
		return def
	}
	return d
}

// bytes accepts a plain byte count or a human suffix: 512kb, 64mb, 4gb.
func (p *parser) bytes(key string, def int64) int64 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	lower := strings.ToLower(strings.ReplaceAll(v, " ", ""))
	mult := int64(1)
	for suffix, m := range map[string]int64{"kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "tb": 1 << 40} {
		if strings.HasSuffix(lower, suffix) {
			lower, mult = strings.TrimSuffix(lower, suffix), m
			break
		}
	}
	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil || n <= 0 {
		p.errf("%s must be a positive byte size such as 4gb or 4294967296, got %q", key, v)
		return def
	}
	return n * mult
}

func (p *parser) enum(key, def string, allowed ...string) string {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	v = strings.ToLower(v)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.errf("%s must be one of %s, got %q", key, strings.Join(allowed, ", "), v)
	return def
}

func (p *parser) list(key string) []string {
	v, ok := p.raw(key)
	if !ok {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p *parser) timezone(key, def string) string {
	v := p.str(key, def)
	if _, err := time.LoadLocation(v); err != nil {
		p.errf("%s must be an IANA timezone such as Europe/Berlin, got %q", key, v)
		return def
	}
	return v
}

// KeyBytes is the length of the AES-256 key ENCORE_ENCRYPTION_KEY must supply.
const KeyBytes = 32

// key decodes a 32-byte AES key. Base64 (standard or URL, padded or not) and hex
// are all accepted because operators reach for whichever their tooling produces.
func (p *parser) key(name string) []byte {
	v, ok := p.raw(name)
	if !ok {
		p.errf("%s is required: generate one with `openssl rand -base64 32`", name)
		return nil
	}
	b, err := decodeKey(v)
	if err != nil {
		p.errf("%s is not a valid key: %v (generate one with `openssl rand -base64 32`)", name, err)
		return nil
	}
	if len(b) != KeyBytes {
		p.errf("%s must decode to exactly %d bytes, got %d", name, KeyBytes, len(b))
		return nil
	}
	return b
}

func decodeKey(v string) ([]byte, error) {
	// Every decoding that succeeds is a candidate, and the one yielding a
	// usable key wins.
	//
	// Order alone cannot decide this: a 64-character hex string is *also* valid
	// base64, and decoding it as base64 gives 48 bytes rather than the intended
	// 32. Trying base64 first therefore rejected every hex key with a confusing
	// "must decode to exactly 32 bytes, got 48", even though hex was documented
	// as supported.
	var candidates [][]byte
	if b, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(v))); err == nil {
		candidates = append(candidates, b)
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(v); err == nil {
			candidates = append(candidates, b)
		}
	}
	for _, b := range candidates {
		if len(b) == KeyBytes {
			return b, nil
		}
	}
	if len(candidates) > 0 {
		// Decodable but the wrong length; hand it back so the caller can say so.
		return candidates[0], nil
	}
	return nil, errors.New("expected base64 or 64 hex characters")
}
