package beacon

import (
	"net/netip"

	"github.com/cruciblelab/crucible-analytic/internal/clientip"
)

// ClientIPResolver is clientip.Resolver under the name this package has
// always used. The rule moved to its own package when the panel needed
// it too (H12); one implementation, so the beacon and the panel cannot
// come to disagree about which hop of X-Forwarded-For to believe.
type ClientIPResolver = clientip.Resolver

// ParseTrustedProxies is clientip.ParseTrustedProxies.
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	return clientip.ParseTrustedProxies(entries)
}
