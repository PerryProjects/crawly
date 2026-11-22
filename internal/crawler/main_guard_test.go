package crawler

import (
	"os"
	"testing"

	"github.com/perryprojects/siteinfo/internal/testguard"
)

// TestMain keeps every test in this package off the network: no test may call the real Firecrawl API.
func TestMain(m *testing.M) {
	testguard.NoExternalNetwork()
	os.Exit(m.Run())
}
