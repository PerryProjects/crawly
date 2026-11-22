// Package testguard keeps tests away from real services: once enabled, HTTP requests through
// http.DefaultTransport may only reach loopback addresses. Tests talk to httptest servers and
// mocks; a test that would call the real Firecrawl API fails instead of spending credits.
// Only test files import this package.
package testguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// NoExternalNetwork replaces http.DefaultTransport with one that refuses non-loopback hosts.
// Call it from TestMain before anything builds an HTTP client.
func NoExternalNetwork() {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dial := (&net.Dialer{}).DialContext
	t.Proxy = nil
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("testguard: tests must not reach %s; use a mock or an httptest server", addr)
		}
		return dial(ctx, network, addr)
	}
	http.DefaultTransport = t
}
