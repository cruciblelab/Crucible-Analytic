package privacy

import (
	"net/netip"
	"testing"
)

func TestMaskIP(t *testing.T) {
	cases := []struct {
		name string
		in   string
		mode IPMode
		want string
	}{
		{"ipv4 masked keeps the network", "185.23.45.178", IPMasked, "185.23.45.0"},
		{"ipv4 masked is idempotent", "185.23.45.0", IPMasked, "185.23.45.0"},
		{"ipv4 full is untouched", "185.23.45.178", IPFull, "185.23.45.178"},
		{"ipv6 masked keeps /64", "2a02:ff0:1234:5678:9abc:def0:1234:5678", IPMasked, "2a02:ff0:1234:5678::"},
		{"ipv6 full is untouched", "2a02:ff0:1234:5678:9abc:def0:1234:5678", IPFull, "2a02:ff0:1234:5678:9abc:def0:1234:5678"},
		{"loopback masks like anything else", "127.0.0.1", IPMasked, "127.0.0.0"},

		// A 4-in-6 address taking the IPv6 path would be "masked" to a
		// /64 that already contains its whole 32-bit address - masked in
		// name only, and the kind of bug that passes every test written
		// in terms of the mode rather than the result.
		{"ipv4-in-ipv6 is unwrapped first", "::ffff:185.23.45.178", IPMasked, "185.23.45.0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskIP(netip.MustParseAddr(tc.in), tc.mode)
			if got.String() != tc.want {
				t.Errorf("MaskIP(%s, %s) = %s, want %s", tc.in, tc.mode, got, tc.want)
			}
		})
	}
}

// The two writers have to agree exactly, or the crossover join silently
// returns nothing. This states the agreement as an equality rather than
// trusting two call sites to stay in step.
func TestMaskIP_IsStableForTheJoin(t *testing.T) {
	// The same visitor seen by the collector and by the beacon, one
	// through a 4-in-6 socket and one not.
	fromCollector := MaskIP(netip.MustParseAddr("::ffff:185.23.45.178"), IPMasked)
	fromBeacon := MaskIP(netip.MustParseAddr("185.23.45.178"), IPMasked)

	if fromCollector != fromBeacon {
		t.Fatalf("the two writers would store %s and %s for one visitor; the crossover join would find nothing",
			fromCollector, fromBeacon)
	}

	// And two visitors in the same /24 collapse to one value. This is
	// the cost of masking, asserted rather than merely documented: the
	// crossover view has to say so, because it can no longer tell these
	// two apart.
	neighbour := MaskIP(netip.MustParseAddr("185.23.45.9"), IPMasked)
	if neighbour != fromBeacon {
		t.Errorf("two addresses in one /24 masked differently: %s and %s", neighbour, fromBeacon)
	}
}

func TestMaskIP_InvalidAddressIsNotInvented(t *testing.T) {
	var zero netip.Addr
	if got := MaskIP(zero, IPMasked); got.IsValid() {
		t.Errorf("an invalid address became %s; masking invented data", got)
	}
}

// An unreadable setting must fall to the safer side. A typo in the
// settings table turning masking off would be the worst possible way for
// this to fail, because nothing would report it.
func TestParseIPMode_FallsBackToMasked(t *testing.T) {
	for _, value := range []string{"", "MASKED", "tam", "maskeli", "off", "no", "full "} {
		if got := ParseIPMode(value); got != IPMasked {
			t.Errorf("ParseIPMode(%q) = %s, want %s", value, got, IPMasked)
		}
	}
	if got := ParseIPMode("full"); got != IPFull {
		t.Errorf("ParseIPMode(\"full\") = %s, want %s", got, IPFull)
	}
	if got := ParseIPMode("masked"); got != IPMasked {
		t.Errorf("ParseIPMode(\"masked\") = %s, want %s", got, IPMasked)
	}
	if DefaultIPMode != IPMasked {
		t.Errorf("the default is %s; legal advice was masked, and the default is what ends up in production", DefaultIPMode)
	}
}
