package privacy

import (
	"bytes"
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
		// Full mode masks the stored address too - what it adds is the
		// keyed token beside it, not a raw address.
		{"ipv4 full still masks the stored address", "185.23.45.178", IPFull, "185.23.45.0"},
		{"ipv6 masked keeps /64", "2a02:ff0:1234:5678:9abc:def0:1234:5678", IPMasked, "2a02:ff0:1234:5678::"},
		{"ipv6 full still masks the stored address", "2a02:ff0:1234:5678:9abc:def0:1234:5678", IPFull, "2a02:ff0:1234:5678::"},
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

// --- full mode's keyed token ---

var testKey = []byte("otuz-iki-baytlik-test-anahtari!!")

// The property the crossover join rests on: equal inputs, equal
// pseudonyms - including across the two spellings of an IPv4 address,
// which the two writers see differently depending on the socket.
func TestHashIP_PreservesEqualityAcrossWriters(t *testing.T) {
	fromCollector := TokenIP(netip.MustParseAddr("::ffff:185.23.45.178"), testKey)
	fromBeacon := TokenIP(netip.MustParseAddr("185.23.45.178"), testKey)

	if len(fromBeacon) != HashLen {
		t.Fatalf("hash is %d bytes, want %d", len(fromBeacon), HashLen)
	}
	if !bytes.Equal(fromCollector, fromBeacon) {
		t.Fatalf("the two writers produced %x and %x for one visitor; the join would find nothing",
			fromCollector, fromBeacon)
	}

	// And unlike the masked address beside it, the token separates two
	// visitors inside one /24. That precision is the whole reason full
	// mode exists - without it the mode would only be masked mode with
	// an extra column.
	if bytes.Equal(TokenIP(netip.MustParseAddr("185.23.45.9"), testKey), fromBeacon) {
		t.Error("two different addresses in one /24 produced the same token; " +
			"full mode has lost the precision it exists for")
	}
	if bytes.Equal(TokenIP(netip.MustParseAddr("185.23.46.178"), testKey), fromBeacon) {
		t.Error("a different network produced the same token")
	}
}

// No mode writes a raw address. Masked mode writes the network; full
// mode writes the same network plus the token. Stated here because it is
// the rule the whole design is built on, and it is the one somebody
// would break by "simplifying" full mode back to storing the address.
func TestModes_NeverStoreARawAddress(t *testing.T) {
	whole := netip.MustParseAddr("185.23.45.178")
	for _, mode := range []IPMode{IPMasked, IPFull} {
		stored := MaskIP(whole, mode)
		if stored == whole {
			t.Errorf("%s stored the raw address %v", mode, stored)
		}
		if stored != netip.MustParseAddr("185.23.45.0") {
			t.Errorf("%s stored %v, want the masked network", mode, stored)
		}
	}
}

// A different deployment must not produce the same tokens, or two
// customers' databases would be joinable to each other.
func TestHashIP_IsKeyed(t *testing.T) {
	other := []byte("bambaska-otuz-iki-baytlik-anaht!")
	if bytes.Equal(TokenIP(netip.MustParseAddr("185.23.45.178"), testKey),
		TokenIP(netip.MustParseAddr("185.23.45.178"), other)) {
		t.Error("two different keys produced the same token")
	}
}

// A missing or short key returns nothing rather than tokenising anyway.
// A weakly keyed value would look like a token and reverse in
// microseconds, which is worse than storing nothing - it would be
// believed.
func TestHashIP_RefusesAWeakKey(t *testing.T) {
	for name, key := range map[string][]byte{
		"nil":     nil,
		"empty":   {},
		"short":   []byte("kisa-anahtar"),
		"one off": make([]byte, MinHashKeyLen-1),
	} {
		t.Run(name, func(t *testing.T) {
			if got := TokenIP(netip.MustParseAddr("185.23.45.178"), key); got != nil {
				t.Errorf("a %s key produced %x", name, got)
			}
		})
	}
	if TokenIP(netip.Addr{}, testKey) != nil {
		t.Error("an invalid address produced a token")
	}
}

// Only full mode needs a key. Masked mode has to work on a deployment
// with nothing configured, because that is what makes the safe option
// the effortless one.
func TestIPMode_OnlyFullNeedsAKey(t *testing.T) {
	if !IPFull.Tokenises() {
		t.Error("full mode does not tokenise, so its key would never be used")
	}
	if IPMasked.Tokenises() {
		t.Error("masked mode tokenises, so it would need a key it should not need")
	}
	if !IPMasked.Masks() || !IPFull.Masks() {
		t.Error("a mode reported that it does not mask; no mode may store a raw address")
	}
	// A retired mode name falls back to the default rather than being
	// honoured as something the code no longer implements.
	if got := ParseIPMode("hashed"); got != IPMasked {
		t.Errorf("ParseIPMode(\"hashed\") = %s, want the default %s", got, IPMasked)
	}
}
