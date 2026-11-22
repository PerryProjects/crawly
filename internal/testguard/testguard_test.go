package testguard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNoExternalNetwork(t *testing.T) {
	NoExternalNetwork()
	t.Run("loopback is allowed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
		defer srv.Close()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	})
	t.Run("the Firecrawl API is refused", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.firecrawl.dev/v2/scrape", http.NoBody)
		_, err := http.DefaultTransport.RoundTrip(req)
		if err == nil || !strings.Contains(err.Error(), "testguard") {
			t.Fatalf("err = %v, want a testguard refusal", err)
		}
	})
}
