package verdict

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

var (
	sharedKey = bytes.Repeat([]byte("k"), privacy.MinHashKeyLen)
	visitor   = netip.MustParseAddr("51.75.10.10")
	at        = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
)

func TestAHeaderTheCollectorWroteIsVerifiedWithItsScore(t *testing.T) {
	key := Key(sharedKey)
	for _, score := range []int{0, scoring.BotCutoff - 1, scoring.BotCutoff, scoring.MaxScore} {
		got, out := Check(key, Sign(key, score, at, visitor), visitor, at)
		if out != Verified || got != score {
			t.Errorf("score %d: Check = (%d, %v), want (%d, Verified)", score, got, out, score)
		}
	}
}

// The threshold below which no verdicts are written is the one TokenIP
// applies, asked of the constant rather than of CanTokenise, so a laxer
// CanTokenise cannot hide behind both sides agreeing.
func TestNoKeyIsDerivedFromAKeyTooShortToTokenise(t *testing.T) {
	if k := Key(sharedKey[:privacy.MinHashKeyLen-1]); k != nil {
		t.Errorf("a %d-byte key derived a verdict key", privacy.MinHashKeyLen-1)
	}
	if k := Key(sharedKey[:privacy.MinHashKeyLen]); k == nil {
		t.Errorf("a %d-byte key derived no verdict key", privacy.MinHashKeyLen)
	}
	if k := Key(nil); k != nil {
		t.Error("no key derived a verdict key")
	}
}

// The derived key is not the shared key: a MAC under one is never a MAC
// under the other.
func TestTheVerdictKeyIsNotTheAddressKey(t *testing.T) {
	if bytes.Equal(Key(sharedKey), sharedKey) {
		t.Fatal("the verdict key is the address key itself")
	}
	h := Sign(sharedKey, 10, at, visitor)
	if _, out := Check(Key(sharedKey), h, visitor, at); out != BadSignature {
		t.Errorf("a header signed under the address key = %v, want BadSignature", out)
	}
}

func TestAHeaderFromAnotherKeyIsABadSignature(t *testing.T) {
	other := Key(bytes.Repeat([]byte("o"), privacy.MinHashKeyLen))
	h := Sign(other, 10, at, visitor)
	if _, out := Check(Key(sharedKey), h, visitor, at); out != BadSignature {
		t.Errorf("Check = %v, want BadSignature", out)
	}
}

// Every field is under the MAC: changing any one of them breaks it.
func TestEveryFieldIsSigned(t *testing.T) {
	key := Key(sharedKey)
	h := Sign(key, 74, at, visitor)
	f := strings.Split(h, " ")
	for i, swap := range map[int]string{
		1: "10",
		2: "1794819601",
		3: "51.75.10.11",
	} {
		g := append([]string(nil), f...)
		g[i] = swap
		if _, out := Check(key, strings.Join(g, " "), visitor, at); out != BadSignature {
			t.Errorf("field %d changed to %q: Check = %v, want BadSignature", i, swap, out)
		}
	}
}

// A header copied from one visitor's request does not hold on another's.
func TestAHeaderFromAnotherAddressDoesNotHold(t *testing.T) {
	key := Key(sharedKey)
	h := Sign(key, 0, at, visitor)
	if _, out := Check(key, h, netip.MustParseAddr("51.75.10.11"), at); out != OtherAddress {
		t.Errorf("Check = %v, want OtherAddress", out)
	}
}

// An IPv4 address and its IPv4-in-IPv6 spelling are one visitor: the
// collector and the beacon may see the two spellings of one connection.
func TestTheTwoSpellingsOfAnIPv4AddressAreOneAddress(t *testing.T) {
	key := Key(sharedKey)
	mapped := netip.AddrFrom16(visitor.As16())
	if _, out := Check(key, Sign(key, 5, at, mapped), visitor, at); out != Verified {
		t.Errorf("signed mapped, checked plain: %v", out)
	}
	if _, out := Check(key, Sign(key, 5, at, visitor), mapped, at); out != Verified {
		t.Errorf("signed plain, checked mapped: %v", out)
	}
}

// Both sides of the skew, at the edge and one second past it.
func TestTheTimeHoldsForMaxSkewEitherWay(t *testing.T) {
	key := Key(sharedKey)
	h := Sign(key, 5, at, visitor)
	for _, c := range []struct {
		now  time.Time
		want Outcome
	}{
		{at.Add(MaxSkew), Verified},
		{at.Add(-MaxSkew), Verified},
		{at.Add(MaxSkew + time.Second), Stale},
		{at.Add(-MaxSkew - time.Second), Stale},
	} {
		if _, out := Check(key, h, visitor, c.now); out != c.want {
			t.Errorf("now = at%+v: Check = %v, want %v", c.now.Sub(at), out, c.want)
		}
	}
}

func TestWhatTheCollectorNeverWritesIsMalformed(t *testing.T) {
	key := Key(sharedKey)
	good := strings.Split(Sign(key, 5, at, visitor), " ")
	with := func(i int, v string) string {
		g := append([]string(nil), good...)
		g[i] = v
		return strings.Join(g, " ")
	}
	for name, h := range map[string]string{
		"one word":           "human",
		"another version":    with(0, "v2"),
		"score not a number": with(1, "x"),
		"score above max":    with(1, "101"),
		"negative score":     with(1, "-1"),
		"padded score":       with(1, "05"),
		"time not a number":  with(2, "now"),
		"padded time":        with(2, "0"+good[2]),
		"not an address":     with(3, "localhost"),
		"six fields":         strings.Join(append(good, "x"), " "),
		"two spaces":         strings.Replace(strings.Join(good, " "), " ", "  ", 1),
	} {
		if _, out := Check(key, h, visitor, at); out != Malformed {
			t.Errorf("%s (%q): Check = %v, want Malformed", name, h, out)
		}
	}
}

func TestNoHeaderIsAbsentNotAPerson(t *testing.T) {
	if s, out := Check(Key(sharedKey), "", visitor, at); out != Absent || s != 0 {
		t.Errorf("Check(\"\") = (%d, %v), want (0, Absent)", s, out)
	}
}
