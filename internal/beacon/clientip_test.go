package beacon

import (
	"net/netip"
	"testing"
)

// mustPrefixes parses trusted-proxy entries for this package's tests. The
// resolver's own tests moved with it to internal/clientip.
func mustPrefixes(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	prefixes, err := ParseTrustedProxies(entries)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", entries, err)
	}
	return prefixes
}
