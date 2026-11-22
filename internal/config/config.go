// Package config loads and validates the service configuration.
//
// Viper is used only inside Load: the result is a plain, validated Config
// struct that is handed to the rest of the program.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const envPrefix = "SITEINFO"

// Config is the complete, typed service configuration.
type Config struct {
	Env       string          `mapstructure:"env"` // "dev" or "production"
	Log       LogConfig       `mapstructure:"log"`
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Firecrawl FirecrawlConfig `mapstructure:"firecrawl"`
	Cache     CacheConfig     `mapstructure:"cache"`
	Protect   ProtectConfig   `mapstructure:"protect"`
	Otel      OtelConfig      `mapstructure:"otel"`
}

// LogConfig configures logging. Sampling applies in production only: within one second the
// first SamplingInitial entries with the same level and message are logged, after that only
// every SamplingThereafter-th. SamplingInitial 0 disables sampling.
type LogConfig struct {
	Level              string `mapstructure:"level"`
	SamplingInitial    int    `mapstructure:"sampling_initial"`
	SamplingThereafter int    `mapstructure:"sampling_thereafter"`
}

// ServerConfig holds HTTP server settings.
//
// Timeout chain (each stage must be longer than the one below it):
// firecrawl.timeout < cache.load_timeout < server.handler_timeout <
// cache.load_timeout + cache.lock_margin (lock expiry).
type ServerConfig struct {
	Addr              string        `mapstructure:"addr"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"`
	HandlerTimeout    time.Duration `mapstructure:"handler_timeout"`
	RetryAfter        time.Duration `mapstructure:"retry_after"`
	ShutdownDelay     time.Duration `mapstructure:"shutdown_delay"`
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
	MaxHeaderBytes    int           `mapstructure:"max_header_bytes"`
	// MaxConnections caps simultaneous TCP connections (0 = unlimited).
	MaxConnections int `mapstructure:"max_connections"`
	// TrustedProxies are CIDRs of reverse proxies whose X-Forwarded-For is believed when
	// determining the client IP. Empty (default): the TCP peer address is used, which is
	// safe but makes every client look like the proxy when one is in front.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// ProtectConfig configures overload and abuse protection. A zero rate or limit disables that check.
// This is defense in depth: volumetric DDoS must be absorbed at the edge (CDN/WAF/ingress).
type ProtectConfig struct {
	IPRate                float64 `mapstructure:"ip_rate"` // requests/s per client IP (IPv6: per /64)
	IPBurst               int     `mapstructure:"ip_burst"`
	TokenRate             float64 `mapstructure:"token_rate"` // requests/s per API token
	TokenBurst            int     `mapstructure:"token_burst"`
	AuthFailuresPerMinute float64 `mapstructure:"auth_failures_per_minute"` // failed logins per IP before it is blocked
	MaxInflightRequests   int     `mapstructure:"max_inflight_requests"`    // concurrent /v1 requests per pod
	MaxTrackedKeys        int     `mapstructure:"max_tracked_keys"`         // bound on limiter memory
	MaxConcurrentLoads    int     `mapstructure:"max_concurrent_loads"`     // concurrent upstream loads per pod
	LoadRate              float64 `mapstructure:"load_rate"`                // upstream loads/s per pod (cache misses)
	LoadBurst             int     `mapstructure:"load_burst"`
}

// OtelConfig configures OpenTelemetry metrics export. The OTLP exporter also honors the
// standard OTEL_EXPORTER_OTLP_* and OTEL_SERVICE_NAME environment variables.
type OtelConfig struct {
	Exporter    string        `mapstructure:"exporter"` // otlp, stdout or none
	Endpoint    string        `mapstructure:"endpoint"` // OTLP/HTTP base URL, e.g. http://otel-collector:4318
	Interval    time.Duration `mapstructure:"interval"`
	ServiceName string        `mapstructure:"service_name"`
}

// AuthConfig configures static bearer token authentication.
type AuthConfig struct {
	Enabled bool     `mapstructure:"enabled"`
	Tokens  []string `mapstructure:"tokens"`
}

// RedisConfig selects and configures the Redis client.
type RedisConfig struct {
	Mode         string        `mapstructure:"mode"` // "cluster" or "single"
	Addrs        []string      `mapstructure:"addrs"`
	Username     string        `mapstructure:"username"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"` // single mode only
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	PoolSize     int           `mapstructure:"pool_size"`
}

// FirecrawlConfig configures the upstream crawler.
type FirecrawlConfig struct {
	APIKey         string        `mapstructure:"api_key"`
	APIURL         string        `mapstructure:"api_url"`
	Timeout        time.Duration `mapstructure:"timeout"`
	MaxConcurrency int           `mapstructure:"max_concurrency"`
	MaxRetries     int           `mapstructure:"max_retries"`
	BackoffBase    time.Duration `mapstructure:"backoff_base"`
}

// CacheConfig configures the read-through cache.
type CacheConfig struct {
	LoadTimeout    time.Duration `mapstructure:"load_timeout"`
	LockMargin     time.Duration `mapstructure:"lock_margin"`
	CloseTimeout   time.Duration `mapstructure:"close_timeout"`
	TTL            TTLConfig     `mapstructure:"ttl"`
	RefreshEnabled bool          `mapstructure:"refresh_enabled"`
	RefreshCool    time.Duration `mapstructure:"refresh_cooldown"`
}

// TTLConfig holds cache lifetimes per entry kind and status.
type TTLConfig struct {
	ImprintOK time.Duration `mapstructure:"imprint_ok"`
	BrandOK   time.Duration `mapstructure:"brand_ok"`
	NotFound  time.Duration `mapstructure:"not_found"`
	Error     time.Duration `mapstructure:"error"`
}

// LockExpiry is the redsync mutex expiry: the load deadline plus a margin.
func (c CacheConfig) LockExpiry() time.Duration { return c.LoadTimeout + c.LockMargin }

// Default returns the configuration used when nothing else is set.
func Default() Config {
	return Config{
		Env: "production",
		Log: LogConfig{Level: "info", SamplingInitial: 100, SamplingThereafter: 50},
		Server: ServerConfig{
			Addr:              ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			HandlerTimeout:    45 * time.Second,
			RetryAfter:        5 * time.Second,
			ShutdownDelay:     5 * time.Second,
			ShutdownTimeout:   50 * time.Second,
			MaxHeaderBytes:    16 << 10,
			MaxConnections:    10000,
		},
		Protect: ProtectConfig{
			IPRate: 50, IPBurst: 100,
			TokenRate: 50, TokenBurst: 100,
			AuthFailuresPerMinute: 10,
			MaxInflightRequests:   256,
			MaxTrackedKeys:        100_000,
			MaxConcurrentLoads:    16,
			LoadRate:              2, LoadBurst: 10,
		},
		Otel: OtelConfig{Exporter: "otlp", Interval: 30 * time.Second, ServiceName: "siteinfo"},
		Auth: AuthConfig{Enabled: true},
		Redis: RedisConfig{
			Mode:         "cluster",
			Addrs:        []string{"localhost:6379"},
			DialTimeout:  3 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
			PoolSize:     20,
		},
		Firecrawl: FirecrawlConfig{
			APIURL:         "https://api.firecrawl.dev",
			Timeout:        30 * time.Second,
			MaxConcurrency: 4,
			MaxRetries:     2,
			BackoffBase:    500 * time.Millisecond,
		},
		Cache: CacheConfig{
			LoadTimeout:  40 * time.Second,
			LockMargin:   10 * time.Second,
			CloseTimeout: 45 * time.Second,
			TTL: TTLConfig{
				ImprintOK: 30 * 24 * time.Hour,
				BrandOK:   30 * 24 * time.Hour,
				NotFound:  24 * time.Hour,
				Error:     5 * time.Minute,
			},
			RefreshEnabled: false,
			RefreshCool:    time.Hour,
		},
	}
}

// Load reads the optional YAML file at path (empty means none), applies
// SITEINFO_* environment overrides, unmarshals once and validates.
//
// Environment keys are the nested config keys joined by "_", for example
// SITEINFO_CACHE_TTL_IMPRINT_OK. Lists are comma separated.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Defaults must be registered key by key so AutomaticEnv knows every key.
	var defaults map[string]any
	if err := mapstructure.Decode(Default(), &defaults); err != nil {
		return nil, fmt.Errorf("config: encode defaults: %w", err)
	}
	setDefaults(v, "", defaults)

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	cfg.Auth.Tokens = cleanList(cfg.Auth.Tokens)
	cfg.Redis.Addrs = cleanList(cfg.Redis.Addrs)
	cfg.Server.TrustedProxies = cleanList(cfg.Server.TrustedProxies)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper, prefix string, m map[string]any) {
	for k, val := range m {
		key := prefix + k
		if sub, ok := val.(map[string]any); ok {
			setDefaults(v, key+".", sub)
			continue
		}
		v.SetDefault(key, val)
	}
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// minTokenLen guards against trivially guessable bearer tokens.
const minTokenLen = 16

// Validate checks value ranges and the timeout chain.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Env != "dev" && c.Env != "production" {
		add("env must be dev or production, got %q", c.Env)
	}
	if c.Log.SamplingInitial < 0 || (c.Log.SamplingInitial > 0 && c.Log.SamplingThereafter < 1) {
		add("log.sampling_initial must be >= 0 and log.sampling_thereafter >= 1 when sampling is on")
	}
	if c.Server.Addr == "" {
		add("server.addr is required")
	}
	if c.Server.MaxHeaderBytes < 4096 {
		add("server.max_header_bytes must be >= 4096")
	}
	if c.Server.MaxConnections < 0 {
		add("server.max_connections must not be negative")
	}
	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			add("server.trusted_proxies: %q is not a CIDR", cidr)
		}
	}
	c.validateProtect(add)
	c.validateOtel(add)
	if c.Auth.Enabled {
		if len(c.Auth.Tokens) == 0 {
			add("auth.tokens must contain at least one token while auth.enabled is true")
		}
		for i, t := range c.Auth.Tokens {
			if len(t) < minTokenLen {
				add("auth.tokens[%d] is shorter than %d characters", i, minTokenLen)
			}
		}
	}
	switch c.Redis.Mode {
	case "cluster", "single":
	default:
		add("redis.mode must be cluster or single, got %q", c.Redis.Mode)
	}
	if len(c.Redis.Addrs) == 0 {
		add("redis.addrs must not be empty")
	}
	if c.Redis.Mode == "cluster" && c.Redis.DB != 0 {
		add("redis.db must be 0 in cluster mode")
	}
	if c.Firecrawl.APIKey == "" && c.Firecrawl.APIURL == Default().Firecrawl.APIURL {
		add("firecrawl.api_key is required for the Firecrawl cloud API (a self-hosted firecrawl.api_url may run without)")
	}
	if c.Firecrawl.MaxConcurrency < 1 {
		add("firecrawl.max_concurrency must be >= 1")
	}
	if c.Firecrawl.MaxRetries < 0 {
		add("firecrawl.max_retries must be >= 0")
	}
	for name, d := range map[string]time.Duration{
		"server.read_header_timeout": c.Server.ReadHeaderTimeout,
		"server.read_timeout":        c.Server.ReadTimeout,
		"server.write_timeout":       c.Server.WriteTimeout,
		"server.idle_timeout":        c.Server.IdleTimeout,
		"server.handler_timeout":     c.Server.HandlerTimeout,
		"server.shutdown_timeout":    c.Server.ShutdownTimeout,
		"firecrawl.timeout":          c.Firecrawl.Timeout,
		"firecrawl.backoff_base":     c.Firecrawl.BackoffBase,
		"cache.load_timeout":         c.Cache.LoadTimeout,
		"cache.lock_margin":          c.Cache.LockMargin,
		"cache.close_timeout":        c.Cache.CloseTimeout,
		"cache.ttl.imprint_ok":       c.Cache.TTL.ImprintOK,
		"cache.ttl.brand_ok":         c.Cache.TTL.BrandOK,
		"cache.ttl.not_found":        c.Cache.TTL.NotFound,
		"cache.ttl.error":            c.Cache.TTL.Error,
		"cache.refresh_cooldown":     c.Cache.RefreshCool,
	} {
		if d <= 0 {
			add("%s must be positive", name)
		}
	}
	if c.Server.ShutdownDelay < 0 || c.Server.RetryAfter < 0 {
		add("server.shutdown_delay and server.retry_after must not be negative")
	}
	if c.Firecrawl.Timeout >= c.Cache.LoadTimeout {
		add("firecrawl.timeout (%s) must be shorter than cache.load_timeout (%s)", c.Firecrawl.Timeout, c.Cache.LoadTimeout)
	}
	if c.Cache.LoadTimeout >= c.Server.HandlerTimeout {
		add("cache.load_timeout (%s) must be shorter than server.handler_timeout (%s)", c.Cache.LoadTimeout, c.Server.HandlerTimeout)
	}
	if c.Server.HandlerTimeout >= c.Cache.LockExpiry() {
		add("server.handler_timeout (%s) must be shorter than the lock expiry cache.load_timeout+cache.lock_margin (%s)",
			c.Server.HandlerTimeout, c.Cache.LockExpiry())
	}
	if c.Server.WriteTimeout <= c.Server.HandlerTimeout {
		add("server.write_timeout (%s) must be longer than server.handler_timeout (%s)", c.Server.WriteTimeout, c.Server.HandlerTimeout)
	}
	return errors.Join(errs...)
}

func (c *Config) validateProtect(add func(string, ...any)) {
	p := c.Protect
	for name, v := range map[string]float64{
		"protect.ip_rate": p.IPRate, "protect.token_rate": p.TokenRate,
		"protect.auth_failures_per_minute": p.AuthFailuresPerMinute, "protect.load_rate": p.LoadRate,
	} {
		if v < 0 {
			add("%s must not be negative", name)
		}
	}
	for name, v := range map[string]int{
		"protect.max_inflight_requests": p.MaxInflightRequests, "protect.max_concurrent_loads": p.MaxConcurrentLoads,
	} {
		if v < 0 {
			add("%s must not be negative", name)
		}
	}
	for _, l := range []struct {
		name  string
		rate  float64
		burst int
	}{{"protect.ip", p.IPRate, p.IPBurst}, {"protect.token", p.TokenRate, p.TokenBurst}, {"protect.load", p.LoadRate, p.LoadBurst}} {
		if l.rate > 0 && l.burst < 1 {
			add("%s_burst must be >= 1 when %s_rate is set", l.name, l.name)
		}
	}
	if p.MaxTrackedKeys < 1000 {
		add("protect.max_tracked_keys must be >= 1000")
	}
}

func (c *Config) validateOtel(add func(string, ...any)) {
	o := c.Otel
	switch o.Exporter {
	case "otlp", "stdout", "none":
	default:
		add("otel.exporter must be otlp, stdout or none, got %q", o.Exporter)
	}
	if o.Interval <= 0 {
		add("otel.interval must be positive")
	}
	if strings.TrimSpace(o.ServiceName) == "" {
		add("otel.service_name must not be empty")
	}
	if o.Endpoint != "" {
		if u, err := url.Parse(o.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("otel.endpoint must be an http(s) URL, got %q", o.Endpoint)
		}
	}
}
