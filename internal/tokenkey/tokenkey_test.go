package tokenkey

import (
	"context"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// Missing's rule, and the empty list that is not an answer.
//
// # Why this file exists at all
//
// Because the package had none, and "no test files" is the shape this
// project has been bitten by: the two readers measure Reports against
// real databases - which is right, since both halves of it are questions
// to PostgreSQL - and that left the one piece of arithmetic here covered
// only from two packages away.
//
// The piece is small and the consequence is not. Missing decides whether
// privacy.ip_storage may be set to full, and the direction that must
// never be wrong is the permissive one.
func TestMissingIsEveryWriterThatDidNotReportAKey(t *testing.T) {
	present := Report{Service: "collector", State: heartbeat.TokenKeyPresent}
	absent := Report{Service: "beacon_writer", State: heartbeat.TokenKeyAbsent}
	unknown := Report{Service: "old_build", State: heartbeat.TokenKeyUnknown}

	for _, tc := range []struct {
		name    string
		in      []Report
		want    []string
		comment string
	}{
		{"nobody at all", nil, nil,
			"an empty list is not 'everybody is ready' - see below"},
		{"one ready writer", []Report{present}, nil, ""},
		{"one writer with no key", []Report{absent}, []string{"beacon_writer"}, ""},
		{"one writer too old to say", []Report{unknown}, []string{"old_build"},
			"unknown refuses, because it is indistinguishable from absent from here"},
		{"ready and not", []Report{present, absent}, []string{"beacon_writer"},
			"one ready writer does not carry the other"},
		{"both kinds of not", []Report{absent, unknown},
			[]string{"beacon_writer", "old_build"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range Missing(tc.in) {
				got = append(got, r.Service)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Missing = %v, want %v (%s)", got, tc.want, tc.comment)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Missing[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	// And the state is carried through, not just the name: both callers
	// pick their sentence from it, and one that arrived as the zero value
	// would send every reader to upgrade a binary.
	missing := Missing([]Report{absent, unknown})
	if len(missing) != 2 {
		t.Fatalf("Missing returned %d reports", len(missing))
	}
	if missing[0].State != heartbeat.TokenKeyAbsent || missing[1].State != heartbeat.TokenKeyUnknown {
		t.Errorf("the states did not survive: %+v", missing)
	}
}

// TestNobodyHavingReportedIsNotEverybodyBeingReady.
//
// The rule Missing deliberately does not carry, asserted here so that the
// asymmetry is written down where somebody reading Missing will find it:
// an empty result means "nothing is misconfigured", and on a deployment
// where no service has ever reported that is *also* what "nothing is
// known" looks like. The two callers have to treat the empty input as a
// refusal of its own, and both do.
//
// Nothing in this package can enforce that, which is why it is a test of
// the *shape* rather than of a return value: Missing(nil) being empty is
// the fact, and the comment beside it is the warning. A future edit that
// made Missing(nil) non-empty would be inventing a service.
func TestNobodyHavingReportedIsNotEverybodyBeingReady(t *testing.T) {
	if got := Missing(nil); len(got) != 0 {
		t.Errorf("Missing(nil) = %+v; it cannot name a service nobody reported", got)
	}
	// Ready is the other half of the same decision, and it is the one
	// place "usable key" is defined.
	if (Report{}).Ready() {
		t.Error("a zero Report reports itself ready; the zero state is unknown, which refuses")
	}
	if !(Report{State: heartbeat.TokenKeyPresent}).Ready() {
		t.Error("a present report is not ready")
	}
}

// TestNoDatabaseIsAnError, not an empty answer.
//
// Reports could have returned nil, nil for a nil pool and left the
// caller to guess. Both callers treat an error as a refusal and an empty
// list as "nobody has spoken" - two different sentences - so handing back
// the second for what is actually neither would put "no service has
// reported yet" in front of an operator whose panel has no database.
func TestNoDatabaseIsAnError(t *testing.T) {
	reports, err := Reports(context.Background(), nil)
	if err == nil {
		t.Fatal("a nil pool answered without an error")
	}
	if reports != nil {
		t.Errorf("a failed call also returned %+v", reports)
	}
	// And it names this package, so an operator reading the log knows
	// which question could not be asked.
	if !strings.HasPrefix(err.Error(), "tokenkey:") {
		t.Errorf("the error does not name this package: %v", err)
	}
}
