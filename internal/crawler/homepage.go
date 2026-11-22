package crawler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"golang.org/x/net/publicsuffix"

	"github.com/perryprojects/siteinfo/internal/observability"
)

// ScrapeHomepage fetches https://<host>/ and treats an error status of the target as ErrUpstream.
// URL normalization strips "www.", but some sites only answer under www (no DNS record or no
// TLS certificate for the bare domain). So when the bare registrable domain fails for site
// reasons, https://www.<host>/ is tried once; the first error is returned if both fail.
// Timeouts, rate limits, account problems and cancellation are returned immediately.
func ScrapeHomepage(ctx context.Context, c Crawler, host string, req ScrapeRequest) (*Page, error) {
	urls := []string{"https://" + host + "/"}
	if root, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil && root == host {
		urls = append(urls, "https://www."+host+"/")
	}
	var firstErr error
	for _, u := range urls {
		p, err := c.Scrape(ctx, u, req)
		if err == nil && p.StatusCode < http.StatusBadRequest {
			return p, nil
		}
		if err == nil {
			// Firecrawl answers HTTP 200 with metadata.statusCode of the target (verified live for a 404).
			err = fmt.Errorf("%w: homepage %s returned HTTP %d", ErrUpstream, u, p.StatusCode)
		}
		if firstErr == nil {
			firstErr = err
		}
		if !errors.Is(err, ErrUpstream) {
			return nil, err
		}
		observability.TraceFrom(ctx).Add("crawler", "homepage failed", "url", u, "error", err)
	}
	return nil, firstErr
}
