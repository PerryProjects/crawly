package crawler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

type mockCrawler struct {
	calls []string
	pages map[string]*Page
	errs  map[string]error
}

func (s *mockCrawler) Scrape(_ context.Context, url string, _ ScrapeRequest) (*Page, error) {
	s.calls = append(s.calls, url)
	if err := s.errs[url]; err != nil {
		return nil, err
	}
	if p, ok := s.pages[url]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: SCRAPE_DNS_RESOLUTION_ERROR", ErrUpstream)
}

func TestScrapeHomepage(t *testing.T) {
	ok := &Page{URL: "https://www.acme.de/", StatusCode: 200}
	tests := []struct {
		name      string
		host      string
		pages     map[string]*Page
		errs      map[string]error
		wantCalls []string
		wantErr   error
	}{
		{"apex works", "acme.de", map[string]*Page{"https://acme.de/": ok}, nil, []string{"https://acme.de/"}, nil},
		{"apex has no DNS, www works", "acme.de", map[string]*Page{"https://www.acme.de/": ok}, nil,
			[]string{"https://acme.de/", "https://www.acme.de/"}, nil},
		{"apex answers 500, www works", "acme.de", map[string]*Page{"https://acme.de/": {StatusCode: 500}, "https://www.acme.de/": ok}, nil,
			[]string{"https://acme.de/", "https://www.acme.de/"}, nil},
		{"both fail", "acme.de", nil, nil, []string{"https://acme.de/", "https://www.acme.de/"}, ErrUpstream},
		{"subdomains get no www variant", "shop.acme.de", nil, nil, []string{"https://shop.acme.de/"}, ErrUpstream},
		{"timeout is not retried", "acme.de", nil, map[string]error{"https://acme.de/": ErrUpstreamTimeout}, []string{"https://acme.de/"}, ErrUpstreamTimeout},
		{"account error is not retried", "acme.de", nil, map[string]error{"https://acme.de/": ErrUpstreamUnavailable}, []string{"https://acme.de/"}, ErrUpstreamUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &mockCrawler{pages: tt.pages, errs: tt.errs}
			p, err := ScrapeHomepage(context.Background(), c, tt.host, ScrapeRequest{})
			if !slices.Equal(c.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", c.calls, tt.wantCalls)
			}
			if tt.wantErr == nil && (err != nil || p == nil) {
				t.Fatalf("p=%v err=%v", p, err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
