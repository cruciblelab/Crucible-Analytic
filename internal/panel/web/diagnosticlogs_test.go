package web

import (
	"reflect"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// TestEveryAddressInTheTextIsShortened, and nothing that only looks like
// one: both halves, because a masker that masked everything would pass
// the first and one that masked nothing would pass the second.
func TestEveryAddressInTheTextIsShortened(t *testing.T) {
	masked := map[string]string{
		"203.0.113.9":                           "203.0.113.0/24",
		"peer 203.0.113.9 refused":              "peer 203.0.113.0/24 refused",
		"dial tcp 198.51.100.7:443: refused":    "dial tcp 198.51.100.0/24:443: refused",
		"from 203.0.113.9.":                     "from 203.0.113.0/24.",
		"a,203.0.113.9,b":                       "a,203.0.113.0/24,b",
		"2001:db8:1:2:3:4:5:6":                  "2001:db8:1:2::/64",
		"[2001:db8:1:2:3:4:5:6]:8443":           "[2001:db8:1:2::/64]:8443",
		"2001:db8:1:2:3::":                      "2001:db8:1:2::/64",
		"::ffff:203.0.113.9":                    "203.0.113.0/24",
		"x-forwarded-for 203.0.113.9, 10.1.2.3": "x-forwarded-for 203.0.113.0/24, 10.1.2.0/24",
	}
	for in, want := range masked {
		if got := maskAddresses(in); got != want {
			t.Errorf("maskAddresses(%q) = %q, want %q", in, got, want)
		}
	}
	kept := []string{
		"", "v0.24.0", "0.24.0", "12:34:56", "2026-09-28T21:03:00Z", "deadbeef",
		"aa:bb:cc:dd:ee:ff", "1.2.3", "schema 24", "took 1.5s",
	}
	for _, in := range kept {
		if got := maskAddresses(in); got != in {
			t.Errorf("maskAddresses(%q) = %q; that is not an address and must be left alone", in, got)
		}
	}
}

// TestAnEmailAddressKeepsItsDomainOnly: which provider refused a message
// is worth sending; whose message it was is not.
func TestAnEmailAddressKeepsItsDomainOnly(t *testing.T) {
	for in, want := range map[string]string{
		"to ali.veli@example.com.tr failed":      "to …@example.com.tr failed",
		"550 <user+tag@mail.example.org>: nope":  "550 <…@mail.example.org>: nope",
		"no address here":                        "no address here",
		"a@b":                                    "a@b",
		"invalid 203.0.113.9 from ali@firma.com": "invalid 203.0.113.0/24 from …@firma.com",
	} {
		if got := maskText(in); got != want {
			t.Errorf("maskText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestALogLineLeavesAsDecided is the owner's option (c) on one line:
// the message and the classified attributes go, addresses shortened, the
// client's claim never, and whatever was not classified is named rather
// than sent.
func TestALogLineLeavesAsDecided(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.FixedZone("TRT", 3*3600))
	got := diagnosticLogLineFrom(panel.LogLine{
		At: at, Service: "beacon_writer", Level: "WARN", Category: "auth",
		Message: "trust decision for 203.0.113.9",
		Site:    "magaza",
		Attrs: map[string]string{
			logging.KeyClaim:   "203.0.113.77",
			logging.KeyPeer:    "203.0.113.9",
			logging.KeyVerdict: "rejected",
			"err":              "bad header from ali@example.com at 198.51.100.4",
			"addr":             "not an address",
			"made_up_key":      "anything",
		},
	})
	want := diagnosticLogLine{
		At: at.UTC(), Service: "beacon_writer", Level: "WARN", Category: "auth",
		Message: "trust decision for 203.0.113.0/24",
		Site:    "magaza",
		Attrs: map[string]string{
			logging.KeyPeer:    "203.0.113.0/24",
			logging.KeyVerdict: "rejected",
			"err":              "bad header from …@example.com at 198.51.100.0/24",
		},
		Withheld: []string{"addr", logging.KeyClaim, "made_up_key"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("line =\n  %+v\nwant\n  %+v", got, want)
	}
}
