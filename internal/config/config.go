// Package config loads and validates the service configuration.
//
// Viper is used only inside Load: the result is a plain, validated Config
// struct that is handed to the rest of the program.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

const envPrefix = "SITEINFO"

// Config is the complete, typed service configuration.
type Config struct {
	Env       string          `mapstructure:"env"` // "dev" or "production"
	LogLevel  string          `mapstructure:"log_level"`
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Firecrawl FirecrawlConfig `mapstructure:"firecrawl"`
	Cache     CacheConfig     `mapstructure:"cache"`
}

// ServerConfig holds HTTP server settings.
//
// Timeout chain (each stage must be longer than the one below it):
// firecrawl.timeout < cache.load_timeout < server.handler_timeout <
// cache.load_timeout + cache.lock_margin (lock expiry).
type ServerConfig struct {
	Addr              string        `mapstructure:"addr"`
	MetricsAddr       string        `mapstructure:"metrics_addr"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"`
	HandlerTimeout    time.Duration `mapstructure:"handler_timeout"`
	RetryAfter        time.Duration `mapstructure:"retry_after"`
	ShutdownDelay     time.Duration `mapstructure:"shutdown_delay"`
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
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
		Env:      "production",
		LogLevel: "info",
		Server: ServerConfig{
			Addr:              ":8080",
			MetricsAddr:       ":9090",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			HandlerTimeout:    45 * time.Second,
			RetryAfter:        5 * time.Second,
			ShutdownDelay:     5 * time.Second,
			ShutdownTimeout:   50 * time.Second,
		},
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
	if c.Server.Addr == "" || c.Server.MetricsAddr == "" {
		add("server.addr and server.metrics_addr are required")
	}
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
