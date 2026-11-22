package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDescribeRedactsSecretsAndFormatsDurations(t *testing.T) {
	c := Default()
	c.Auth.Tokens = []string{"super-secret-token-123456"}
	c.Firecrawl.APIKey = "fc-very-secret"
	c.Redis.Password = "redis-secret"
	d := c.Describe()

	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"super-secret-token", "fc-very-secret", "redis-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("secret %q leaked: %s", secret, b)
		}
	}
	if d["auth"].(map[string]any)["tokens"] != "***" || d["firecrawl"].(map[string]any)["api_key"] != "***" {
		t.Errorf("secrets must be masked: %v", d["auth"])
	}
	if d["redis"].(map[string]any)["username"] != "" {
		t.Errorf("non-secret empty values stay as they are")
	}
	if got := d["server"].(map[string]any)["handler_timeout"]; got != "45s" {
		t.Errorf("handler_timeout = %v, want the string 45s", got)
	}
	if got := d["cache"].(map[string]any)["ttl"].(map[string]any)["imprint_ok"]; got != "720h0m0s" {
		t.Errorf("nested duration = %v", got)
	}
	empty := Default()
	if empty.Describe()["firecrawl"].(map[string]any)["api_key"] != "(unset)" {
		t.Error("an unset secret must say so")
	}
}
