package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef-token"

func TestLoadDefaultsAndEnv(t *testing.T) {
	t.Setenv("SITEINFO_AUTH_TOKENS", testToken+", second-token-0123456789")
	t.Setenv("SITEINFO_CACHE_TTL_IMPRINT_OK", "48h")
	t.Setenv("SITEINFO_REDIS_MODE", "single")
	t.Setenv("SITEINFO_REDIS_ADDRS", "redis:6379")
	t.Setenv("SITEINFO_FIRECRAWL_MAX_CONCURRENCY", "7")

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
	if len(cfg.Auth.Tokens) != 2 || cfg.Auth.Tokens[1] != "second-token-0123456789" {
		t.Errorf("tokens = %q", cfg.Auth.Tokens)
	}
	if cfg.Redis.Mode != "single" || cfg.Redis.Addrs[0] != "redis:6379" {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.Firecrawl.MaxConcurrency != 7 {
		t.Errorf("max concurrency = %d", cfg.Firecrawl.MaxConcurrency)
	}
}

func TestLoadFileThenEnvWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	yaml := "auth:\n  enabled: false\nserver:\n  addr: \":9999\"\ncache:\n  ttl:\n    error: 1m\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SITEINFO_SERVER_ADDR", ":7777")
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

func TestValidate(t *testing.T) {
	valid := func() Config {
		c := Default()
		c.Auth.Tokens = []string{testToken}
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
		{"firecrawl >= load", func(c *Config) { c.Firecrawl.Timeout = 41 * time.Second }, "firecrawl.timeout"},
		{"load >= handler", func(c *Config) { c.Cache.LoadTimeout = 46 * time.Second }, "cache.load_timeout"},
		{"handler >= lock expiry", func(c *Config) { c.Cache.LockMargin = time.Second }, "lock expiry"},
		{"write <= handler", func(c *Config) { c.Server.WriteTimeout = 45 * time.Second }, "write_timeout"},
		{"zero ttl", func(c *Config) { c.Cache.TTL.Error = 0 }, "cache.ttl.error"},
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
