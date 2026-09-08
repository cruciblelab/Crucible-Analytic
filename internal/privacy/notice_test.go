package privacy

import (
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a disclosure test has to be careful about.
//
// The easy version of this file asserts that NewNotice(IPMasked) returns
// the fields NewNotice(IPMasked) returns. That passes forever, including
// on the day somebody types the wrong number into the notice, because
// the expected values were copied from the same place the notice reads.
//
// So the expectations here are measured rather than quoted. The prefix
// lengths come from running MaskIP on an address with every bit set and
// counting what survived; the tokenisation claim is checked against
// Tokenises, which is the predicate the writer itself branches on. A
// notice that hard-coded "/24" would pass on today's constants and fail
// the moment the masking changed - which is precisely the failure worth
// catching, because it is the one that sends a visitor a sentence about
// a system that no longer exists.

// someRotation is an arbitrary period, and arbitrary on purpose.
//
// This package does not know how often the beacon rotates its salt and
// must not: the number lives in internal/beacon.DefaultSaltPeriod, is
// configurable there, and a copy of it here would be a second answer to
// a question with one. What the notice does with the period it is given
// has its own test below; everywhere else it is scaffolding.
const someRotation = 24 * time.Hour

// keptBits counts the leading bits MaskIP leaves standing.
//
// Measured from behaviour, not read from maskedIPv4Bits: this file is
// checking that the disclosure agrees with the masking, and a check that
// read the same constant the masking reads would agree with itself.
func keptBits(t *testing.T, all string) int {
	t.Helper()
	masked := MaskIP(netip.MustParseAddr(all), IPMasked)
	kept := 0
	for _, b := range masked.AsSlice() {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<bit) == 0 {
				return kept
			}
			kept++
		}
	}
	return kept
}

// TestTheNoticeNamesThePrefixLengthsTheMaskingActuallyKeeps.
func TestTheNoticeNamesThePrefixLengthsTheMaskingActuallyKeeps(t *testing.T) {
	v4 := keptBits(t, "255.255.255.255")
	v6 := keptBits(t, "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	if v4 == 0 || v6 == 0 {
		t.Fatalf("MaskIP kept %d IPv4 bits and %d IPv6 bits; the measurement is broken, "+
			"so nothing below would mean anything", v4, v6)
	}

	for _, mode := range []IPMode{IPMasked, IPFull} {
		said := NewNotice(mode, someRotation).AddressMaskedTo
		for _, want := range []int{v4, v6} {
			if !strings.Contains(said, "/"+strconv.Itoa(want)) {
				t.Errorf("in %s mode the notice says %q, but MaskIP keeps /%d.\n"+
					"A visitor reading this is being told about a system that does "+
					"something else with their address", mode, said, want)
			}
		}
	}
}

// TestBothModesMaskTheStoredAddressToTheSameLength.
//
// The sentence "we never write your address" is true in both modes and
// true for different reasons, and the difference is a token - not a
// longer prefix. If full mode ever started keeping more of the address,
// the masked-mode text would still be right and the full-mode text would
// be a lie, so the equality is asserted rather than assumed.
func TestBothModesMaskTheStoredAddressToTheSameLength(t *testing.T) {
	masked := NewNotice(IPMasked, someRotation)
	full := NewNotice(IPFull, someRotation)

	if masked.AddressMaskedTo != full.AddressMaskedTo {
		t.Errorf("masked mode says %q and full mode says %q; one of those two "+
			"sentences is now wrong", masked.AddressMaskedTo, full.AddressMaskedTo)
	}

	// And the claim is not vacuous: full mode really does keep only the
	// network, measured on an address rather than argued from names.
	whole := netip.MustParseAddr("185.23.45.178")
	if got := MaskIP(whole, IPFull); got.String() != "185.23.45.0" {
		t.Errorf("full mode stores %s for %s; the notice claims the address is "+
			"never written", got, whole)
	}
}

// TestTheTwoModesProduceTwoDifferentNotices is the phase's headline
// claim: two modes, two texts, both derived.
//
// Different in exactly the fields that describe the difference, and
// identical everywhere else. Half of that is worth as much as the other:
// a notice that changed its rotation period when somebody switched to
// full mode would be describing a system nobody built.
func TestTheTwoModesProduceTwoDifferentNotices(t *testing.T) {
	masked := NewNotice(IPMasked, someRotation)
	full := NewNotice(IPFull, someRotation)

	if masked == full {
		t.Fatal("the two modes produce the same notice, so switching to full " +
			"precision would leave every visitor reading the masked-mode text")
	}

	// The difference, and it is checked against Tokenises rather than
	// against the mode's name: Tokenises is the predicate storedPseudonym
	// branches on, so this is the disclosure agreeing with the writer.
	for _, tc := range []struct{ mode IPMode }{{IPMasked}, {IPFull}} {
		n := NewNotice(tc.mode, someRotation)
		if n.TokenFromWholeAddress != tc.mode.Tokenises() {
			t.Errorf("in %s mode the notice says a token is stored: %v; the writer "+
				"stores one: %v", tc.mode, n.TokenFromWholeAddress, tc.mode.Tokenises())
		}
		// The capability is what a visitor cares about, and it is the
		// token that buys it. Stated as an equality so neither can drift
		// into claiming something the other does not.
		if n.TellsVisitorsApartWithinANetwork != n.TokenFromWholeAddress {
			t.Errorf("in %s mode the notice stores a token (%v) but claims to tell "+
				"visitors apart within a network (%v)", tc.mode,
				n.TokenFromWholeAddress, n.TellsVisitorsApartWithinANetwork)
		}
	}

	// Everything else agrees. Written as a copy with the two known
	// differences erased rather than as a field-by-field list, so a
	// field added later is covered without anybody remembering to add it
	// here - the same reason the rest of this repository derives lists
	// instead of typing names.
	a, b := masked, full
	a.IPStorage, b.IPStorage = "", ""
	a.TokenFromWholeAddress, b.TokenFromWholeAddress = false, false
	a.TellsVisitorsApartWithinANetwork, b.TellsVisitorsApartWithinANetwork = false, false
	if a != b {
		t.Errorf("switching the mode changed something other than the token:\n"+
			" masked: %+v\n   full: %+v", a, b)
	}
}

// TestTheNoticeCarriesTheRotationItWasGiven.
//
// Given rather than read from a constant, because the beacon's period is
// configurable and a disclosure quoting 24 hours at a deployment that
// rotates every 6 would be wrong in the direction that flatters us.
func TestTheNoticeCarriesTheRotationItWasGiven(t *testing.T) {
	for _, want := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 36 * time.Hour} {
		if got := NewNotice(IPMasked, want).IdentifierRotatesEvery; got != want {
			t.Errorf("given %s the notice reports %s", want, got)
		}
	}
}

// TestTheStandingClaimsHoldInEveryMode.
//
// Four sentences the page prints without a condition. Each of them is a
// promise to a visitor, so each is checked in both modes rather than in
// whichever one the test happened to build.
func TestTheStandingClaimsHoldInEveryMode(t *testing.T) {
	for _, mode := range []IPMode{IPMasked, IPFull} {
		n := NewNotice(mode, someRotation)
		if n.Cookies {
			t.Errorf("%s mode reports cookies; nothing in this product sets one", mode)
		}
		if n.DeletionOnRequest {
			t.Errorf("%s mode offers deletion on request, which there is no way to honour", mode)
		}
		if n.NoDeletionReason != NoDeletionNoIdentity {
			t.Errorf("%s mode gives the reason as %q; the page renders a sentence for "+
				"%q and would show a visitor nothing at all",
				mode, n.NoDeletionReason, NoDeletionNoIdentity)
		}
		if n.OptOut != OptOutCall {
			t.Errorf("%s mode tells a visitor to run %q; beacon.js defines %q",
				mode, n.OptOut, OptOutCall)
		}
	}
}

// TestAModeNobodyImplementsIsReportedAsTheOneInForce.
//
// The mode reaches this from a settings row, which is a place a wrong
// value can come from: an older build, a hand-edited database, a typo in
// a config file. The writer already resolves an unknown value to masked
// - ParseIPMode is what both call - and the notice has to report what
// the writer will do, not what the row says. The dangerous direction is
// the other one: a notice echoing "full" for junk would tell a visitor
// more is stored than is, and a notice echoing junk verbatim would put
// an unknown word on a public page.
func TestAModeNobodyImplementsIsReportedAsTheOneInForce(t *testing.T) {
	for _, junk := range []IPMode{"", "Full", "raw", "masked "} {
		n := NewNotice(junk, someRotation)
		if n.IPStorage != DefaultIPMode {
			t.Errorf("mode %q is disclosed as %q; the writer would treat it as %q",
				junk, n.IPStorage, DefaultIPMode)
		}
		if n.TokenFromWholeAddress {
			t.Errorf("mode %q is disclosed as storing a token; the writer stores none", junk)
		}
	}
}
