// Package config loads the service configuration from the environment.
//
// It is hand rolled rather than pulled from a library: the surface is small,
// every field gets a documented default, and validation errors are collected
// so a misconfigured deployment reports everything that is wrong at once
// instead of one variable per restart.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved configuration of the API service.
type Config struct {
	HTTP     HTTP
	Postgres Postgres
	Redis    Redis
	Cache    Cache
	Limiter  Limiter
	Clicks   Clicks
	Log      Log

	// BaseURL is the public origin used to render short links in API
	// responses, e.g. https://sho.rt.
	BaseURL string
	// CodeLength is the number of characters in a generated short code.
	CodeLength int
	// IPHashSalt is mixed into visitor IP hashes so that the stored digests
	// cannot be reversed with a rainbow table of the IPv4 space.
	IPHashSalt string
	// TrustProxyHeaders makes the service read the client address from
	// X-Forwarded-For instead of the socket. Enable it only when a proxy that
	// overwrites the header really does sit in front: the header is
	// client-controlled, so trusting it on a directly exposed service lets
	// anyone forge an address and walk around the per-IP rate limit.
	TrustProxyHeaders bool
}

// HTTP configures the public HTTP server.
type HTTP struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// Postgres configures the connection pool.
type Postgres struct {
	DSN      string
	MaxConns int32
}

// Redis configures the cache and rate limiter backend.
type Redis struct {
	Addr     string
	Password string
	DB       int
}

// Cache configures the link cache TTLs.
type Cache struct {
	// TTL is how long a resolved link stays cached.
	TTL time.Duration
	// NegativeTTL is how long a "no such code" answer stays cached. Caching
	// misses is what keeps a flood of random codes from reaching Postgres.
	NegativeTTL time.Duration
}

// Limiter configures the token bucket rate limiter.
type Limiter struct {
	RPS   float64
	Burst int
}

// Clicks configures the asynchronous analytics collector.
type Clicks struct {
	BufferSize    int
	Workers       int
	BatchSize     int
	FlushInterval time.Duration
}

// Log configures the structured logger.
type Log struct {
	Level  string
	Format string
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := &Config{
		HTTP: HTTP{
			Addr:            envString("HTTP_ADDR", ":8080"),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 5*time.Second, collect),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 10*time.Second, collect),
			IdleTimeout:     envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second, collect),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second, collect),
		},
		Postgres: Postgres{
			DSN:      envString("POSTGRES_DSN", "postgres://shortlink:shortlink@localhost:5432/shortlink?sslmode=disable"),
			MaxConns: envInt32("POSTGRES_MAX_CONNS", 10, collect),
		},
		Redis: Redis{
			Addr:     envString("REDIS_ADDR", "localhost:6379"),
			Password: envString("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0, collect),
		},
		Cache: Cache{
			TTL:         envDuration("CACHE_TTL", time.Hour, collect),
			NegativeTTL: envDuration("CACHE_NEGATIVE_TTL", 30*time.Second, collect),
		},
		Limiter: Limiter{
			RPS:   envFloat("RATE_LIMIT_RPS", 20, collect),
			Burst: envInt("RATE_LIMIT_BURST", 40, collect),
		},
		Clicks: Clicks{
			BufferSize:    envInt("CLICKS_BUFFER_SIZE", 10_000, collect),
			Workers:       envInt("CLICKS_WORKERS", 2, collect),
			BatchSize:     envInt("CLICKS_BATCH_SIZE", 500, collect),
			FlushInterval: envDuration("CLICKS_FLUSH_INTERVAL", 2*time.Second, collect),
		},
		Log: Log{
			Level:  strings.ToLower(envString("LOG_LEVEL", "info")),
			Format: strings.ToLower(envString("LOG_FORMAT", "json")),
		},
		BaseURL:           strings.TrimRight(envString("BASE_URL", "http://localhost:8080"), "/"),
		CodeLength:        envInt("CODE_LENGTH", 7, collect),
		IPHashSalt:        envString("IP_HASH_SALT", ""),
		TrustProxyHeaders: envBool("TRUST_PROXY_HEADERS", false, collect),
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadDatabase reads only the database settings.
//
// The operator CLI needs a connection and nothing else; making it satisfy the
// whole server configuration -- an IP hash salt, a base URL, rate limits --
// would be a barrier with no purpose, and would tempt an operator into setting
// fake values just to run a command.
func LoadDatabase() (*Postgres, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := &Postgres{
		DSN:      envString("POSTGRES_DSN", "postgres://shortlink:shortlink@localhost:5432/shortlink?sslmode=disable"),
		MaxConns: envInt32("POSTGRES_MAX_CONNS", 10, collect),
	}
	if cfg.DSN == "" {
		errs = append(errs, errors.New("POSTGRES_DSN must be set"))
	}
	if cfg.MaxConns <= 0 {
		errs = append(errs, errors.New("POSTGRES_MAX_CONNS must be positive"))
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Bot is the configuration of the Telegram bot process.
type Bot struct {
	Postgres Postgres
	Log      Log

	TelegramToken  string
	CryptoPayToken string
	// CryptoPayURL is the provider base URL, derived from CRYPTOPAY_NETWORK.
	CryptoPayURL string
	Network      string

	PriceUSD       string
	AccessDays     int
	MaxKeys        int
	AcceptedAssets []string
	InvoiceTTL     time.Duration

	// ReconcileInterval is how often pending invoices are checked with the
	// provider, which covers lost webhooks and local development alike.
	ReconcileInterval time.Duration
	// HTTPAddr serves the payment webhook.
	HTTPAddr string
	// APIBaseURL is shown to buyers as the address of the API.
	APIBaseURL string
	// Timezone is the zone dates are shown to buyers in.
	Timezone string
}

// LoadBot reads the bot configuration from the environment.
func LoadBot() (*Bot, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := &Bot{
		Postgres: Postgres{
			DSN:      envString("POSTGRES_DSN", "postgres://shortlink:shortlink@localhost:5432/shortlink?sslmode=disable"),
			MaxConns: envInt32("POSTGRES_MAX_CONNS", 5, collect),
		},
		Log: Log{
			Level:  strings.ToLower(envString("LOG_LEVEL", "info")),
			Format: strings.ToLower(envString("LOG_FORMAT", "json")),
		},
		TelegramToken:  envString("TELEGRAM_BOT_TOKEN", ""),
		CryptoPayToken: envString("CRYPTOPAY_TOKEN", ""),
		// Testnet by default: a bot started without thinking about it must not
		// be able to take real money. Mainnet is always a deliberate choice.
		Network:           strings.ToLower(envString("CRYPTOPAY_NETWORK", "testnet")),
		PriceUSD:          envString("PRICE_USD", "10"),
		AccessDays:        envInt("ACCESS_DAYS", 5, collect),
		MaxKeys:           envInt("MAX_KEYS_PER_USER", 5, collect),
		AcceptedAssets:    splitList(envString("CRYPTOPAY_ASSETS", "USDT,TON")),
		InvoiceTTL:        envDuration("INVOICE_TTL", time.Hour, collect),
		ReconcileInterval: envDuration("RECONCILE_INTERVAL", 20*time.Second, collect),
		HTTPAddr:          envString("BOT_HTTP_ADDR", ":8090"),
		APIBaseURL:        strings.TrimRight(envString("BASE_URL", "http://localhost:8080"), "/"),
		Timezone:          envString("BOT_TIMEZONE", "Europe/Kyiv"),
	}

	switch cfg.Network {
	case "testnet":
		cfg.CryptoPayURL = "https://testnet-pay.crypt.bot"
	case "mainnet":
		cfg.CryptoPayURL = "https://pay.crypt.bot"
	default:
		errs = append(errs, fmt.Errorf("CRYPTOPAY_NETWORK must be testnet or mainnet, got %q", cfg.Network))
	}

	require := func(ok bool, problem string) {
		if !ok {
			errs = append(errs, errors.New(problem))
		}
	}
	require(cfg.TelegramToken != "", "TELEGRAM_BOT_TOKEN must be set")
	require(cfg.CryptoPayToken != "", "CRYPTOPAY_TOKEN must be set")
	require(cfg.Postgres.DSN != "", "POSTGRES_DSN must be set")
	require(isPositiveAmount(cfg.PriceUSD), "PRICE_USD must be a positive amount such as 10 or 9.99")
	require(cfg.AccessDays > 0, "ACCESS_DAYS must be positive")
	require(cfg.MaxKeys > 0, "MAX_KEYS_PER_USER must be positive")
	require(len(cfg.AcceptedAssets) > 0, "CRYPTOPAY_ASSETS must list at least one asset")
	require(cfg.InvoiceTTL >= time.Minute, "INVOICE_TTL must be at least 1m")
	require(cfg.ReconcileInterval >= time.Second, "RECONCILE_INTERVAL must be at least 1s")

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// isPositiveAmount accepts a plain decimal with at most two fractional digits.
// The price is sent to the provider verbatim, so it is checked here rather
// than discovered as a rejected invoice at the first sale.
func isPositiveAmount(s string) bool {
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || (hasFrac && (frac == "" || len(frac) > 2)) {
		return false
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return false
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	return err == nil && v > 0
}

func (c *Config) validate() error {
	var errs []error
	require := func(ok bool, problem string) {
		if !ok {
			errs = append(errs, errors.New(problem))
		}
	}

	require(c.Postgres.DSN != "", "POSTGRES_DSN must be set")
	require(c.Postgres.MaxConns > 0, "POSTGRES_MAX_CONNS must be positive")
	require(c.Redis.Addr != "", "REDIS_ADDR must be set")
	require(c.BaseURL != "", "BASE_URL must be set")
	require(c.CodeLength >= 4 && c.CodeLength <= 32, "CODE_LENGTH must be between 4 and 32")
	require(c.Cache.TTL > 0, "CACHE_TTL must be positive")
	require(c.Cache.NegativeTTL > 0, "CACHE_NEGATIVE_TTL must be positive")
	require(c.Limiter.RPS > 0, "RATE_LIMIT_RPS must be positive")
	require(c.Limiter.Burst > 0, "RATE_LIMIT_BURST must be positive")
	require(c.Clicks.BufferSize > 0, "CLICKS_BUFFER_SIZE must be positive")
	require(c.Clicks.Workers > 0, "CLICKS_WORKERS must be positive")
	require(c.Clicks.BatchSize > 0, "CLICKS_BATCH_SIZE must be positive")
	require(c.Clicks.FlushInterval > 0, "CLICKS_FLUSH_INTERVAL must be positive")
	// An empty salt would make visitor hashes a plain SHA-256 of the IP, which
	// is reversible by brute force over the address space in seconds.
	require(c.IPHashSalt != "", "IP_HASH_SALT must be set")

	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text, got %q", c.Log.Format))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", c.Log.Level))
	}

	return errors.Join(errs...)
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, collect func(error)) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not an integer", key, v))
		return def
	}
	return n
}

// envInt32 parses at 32 bits so an out-of-range value is rejected here rather
// than silently wrapping on conversion.
func envInt32(key string, def int32, collect func(error)) int32 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a 32-bit integer", key, v))
		return def
	}
	return int32(n)
}

func envBool(key string, def bool, collect func(error)) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a boolean (try true or false)", key, v))
		return def
	}
	return b
}

func envFloat(key string, def float64, collect func(error)) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a number", key, v))
		return def
	}
	return f
}

func envDuration(key string, def time.Duration, collect func(error)) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a duration (try 5s, 1h)", key, v))
		return def
	}
	return d
}
