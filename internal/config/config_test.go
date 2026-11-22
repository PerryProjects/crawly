package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef-token"

func testLoadDefaultsAndEnv(t *testing.T) {
	t.Setenv("SITEINFO_AUTH_TOKENS", testToken+", second-token-0123456789abcdef0123456789")
	t.Setenv("SITEINFO_FIRECRAWL_API_KEY", "fc-test")
	t.Setenv("SITEINFO_CACHE_TTL_IMPRINT_OK", "48h")
	t.Setenv("SITEINFO_REDIS_MODE", "single")
	t.Setenv("SITEINFO_REDIS_ADDRS", "redis:6379")
	t.Setenv("SITEINFO_FIRECRAWL_MAX_CONCURRENCY", "7")
	t.Setenv("SITEINFO_PROTECT_LOAD_RATE", "12.5")
	t.Setenv("SITEINFO_LOG_LEVEL", "warn")
	t.Setenv("SITEINFO_FIRECRAWL_LANGUAGES", "de, en")
	t.Setenv("SITEINFO_LOG_SAMPLING_THEREAFTER", "20")
	t.Setenv("SITEINFO_OTEL_ENDPOINT", "http://collector:4318")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Cache.TTL.ImprintOK; got != 48*time.Hour {
		t.Errorf("imprint ttl = %s", got)
	}
	if cfg.Cache.TTL.NotFound != 24*time.Hour {
		t.Errorf("default not_found ttl lost: %s", cfg.Cache.TTL.NotFound)
	}
	if len(cfg.Auth.Tokens) != 2 || cfg.Auth.Tokens[1] != "second-token-0123456789abcdef0123456789" {
		t.Errorf("tokens = %q", cfg.Auth.Tokens)
	}
	if cfg.Redis.Mode != "single" || cfg.Redis.Addrs[0] != "redis:6379" {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.Firecrawl.MaxConcurrency != 7 {
		t.Errorf("max concurrency = %d", cfg.Firecrawl.MaxConcurrency)
	}
	if cfg.Firecrawl.Country != "DE" || len(cfg.Firecrawl.Languages) != 2 || cfg.Firecrawl.Languages[1] != "en" {
		t.Errorf("firecrawl location = %q %q", cfg.Firecrawl.Country, cfg.Firecrawl.Languages)
	}
	if cfg.Log.Level != "warn" || cfg.Log.SamplingInitial != 100 || cfg.Log.SamplingThereafter != 20 {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.Protect.LoadRate != 12.5 || cfg.Protect.LoadBurst != 10 {
		t.Errorf("protect = %+v", cfg.Protect)
	}
	if cfg.Otel.Endpoint != "http://collector:4318" || cfg.Otel.Exporter != "otlp" {
		t.Errorf("otel = %+v", cfg.Otel)
	}
}

func testLoadFileThenEnvWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	yaml := "auth:\n  enabled: false\nserver:\n  addr: \":9999\"\ncache:\n  ttl:\n    error: 1m\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SITEINFO_SERVER_ADDR", ":7777")
	t.Setenv("SITEINFO_FIRECRAWL_API_URL", "http://firecrawl.internal:3002")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":7777" {
		t.Errorf("env should override file, got %q", cfg.Server.Addr)
	}
	if cfg.Cache.TTL.Error != time.Minute || cfg.Auth.Enabled {
		t.Errorf("file values lost: %+v %+v", cfg.Cache.TTL, cfg.Auth)
	}
}

func testValidate(t *testing.T) {
	valid := func() Config {
		c := Default()
		c.Auth.Tokens = []string{testToken}
		c.Firecrawl.APIKey = "fc-test"
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"no tokens", func(c *Config) { c.Auth.Tokens = nil }, "auth.tokens"},
		{"auth disabled without tokens", func(c *Config) { c.Auth.Enabled = false; c.Auth.Tokens = nil }, ""},
		{"short token", func(c *Config) { c.Auth.Tokens = []string{"abc"} }, "shorter than"},
		{"bad mode", func(c *Config) { c.Redis.Mode = "x" }, "redis.mode"},
		{"firecrawl >= load", func(c *Config) { c.Firecrawl.Timeout = 61 * time.Second }, "firecrawl.timeout"},
		{"branding >= load", func(c *Config) { c.Firecrawl.BrandingTimeout = 60 * time.Second }, "branding_timeout"},
		{"load >= handler", func(c *Config) { c.Cache.LoadTimeout = 66 * time.Second }, "cache.load_timeout"},
		{"zero lock ttl", func(c *Config) { c.Cache.LockTTL = 0 }, "cache.lock_ttl"},
		{"write <= handler", func(c *Config) { c.Server.WriteTimeout = 45 * time.Second }, "write_timeout"},
		{"zero ttl", func(c *Config) { c.Cache.TTL.Error = 0 }, "cache.ttl.error"},
		{"no api key for cloud", func(c *Config) { c.Firecrawl.APIKey = "" }, "firecrawl.api_key"},
		{"no api key for self-hosted", func(c *Config) { c.Firecrawl.APIKey = ""; c.Firecrawl.APIURL = "http://fc:3002" }, ""},
		{"sampling off", func(c *Config) { c.Log.SamplingInitial, c.Log.SamplingThereafter = 0, 0 }, ""},
		{"sampling needs thereafter", func(c *Config) { c.Log.SamplingThereafter = 0 }, "sampling_thereafter"},
		{"country off", func(c *Config) { c.Firecrawl.Country = "" }, ""},
		{"bad country", func(c *Config) { c.Firecrawl.Country = "de" }, "firecrawl.country"},
		{"burst missing", func(c *Config) { c.Protect.LoadRate, c.Protect.LoadBurst = 5, 0 }, "load_burst"},
		{"rate disabled needs no burst", func(c *Config) { c.Protect.LoadRate, c.Protect.LoadBurst = 0, 0 }, ""},
		{"negative limit", func(c *Config) { c.Protect.MaxInflightRequests = -1 }, "max_inflight_requests"},
		{"bad exporter", func(c *Config) { c.Otel.Exporter = "prometheus" }, "otel.exporter"},
		{"bad otel endpoint", func(c *Config) { c.Otel.Endpoint = "collector:4318" }, "otel.endpoint"},
		{"good otel endpoint", func(c *Config) { c.Otel.Endpoint = "http://collector:4318" }, ""},
		{"small header limit", func(c *Config) { c.Server.MaxHeaderBytes = 100 }, "max_header_bytes"},
		{"bad env", func(c *Config) { c.Env = "staging" }, "env must"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error %v does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfig(t *testing.T) {
	t.Run("load defaults and env", testLoadDefaultsAndEnv)
	t.Run("load file then env wins", testLoadFileThenEnvWins)
	t.Run("validate", testValidate)
}
