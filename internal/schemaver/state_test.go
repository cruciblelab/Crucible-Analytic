// The three predicates, tested where they live.
//
// # Why this file exists separately from the page test
//
// A mutation survived, and finding out why was the useful part. Making
// Matches() return true for an unrecorded database did not fail the
// health page's tests - because healthSchema returns early on an
// unrecorded state and never calls Matches() at all. The page was
// protected by an accident of control flow, not by the predicate being
// right.
//
// That is exactly the kind of green worth distrusting. The predicate is
// still wrong, and the callers that will meet it are the ones not
// written yet: L2 asks "does this schema match" before the first write,
// and L3 asks it before offering an upgrade. Either would have been
// handed `true` for a database that has never recorded a version.
//
// So the state machine is tested here, directly, rather than only
// through a page that happens not to reach the broken branch.
package schemaver

import "testing"

func TestTheThreePredicates(t *testing.T) {
	const other = "0000000000000000000000000000000000000000000000000000000000000000"

	for _, tc := range []struct {
		name                   string
		state                  State
		matches, ahead, behind bool
		why                    string
	}{
		{
			name:    "tam olarak beklenen",
			state:   State{Recorded: true, Version: Version, Fingerprint: Fingerprint},
			matches: true,
			why:     "the only state that should read as a match",
		},
		{
			// The state a version-only check cannot see, and the reason
			// the fingerprint exists at all.
			name:    "sürüm aynı, şema başka",
			state:   State{Recorded: true, Version: Version, Fingerprint: other},
			matches: false,
			why:     "same number, different schema - what a half-applied upgrade leaves",
		},
		{
			name:  "veritabanı geride",
			state: State{Recorded: true, Version: Version - 1, Fingerprint: other},
			ahead: true,
			why:   "the direction that loses rows",
		},
		{
			name:   "veritabanı ileride",
			state:  State{Recorded: true, Version: Version + 1, Fingerprint: other},
			behind: true,
			why:    "safe, but somebody rolled a binary back",
		},
		{
			// The mutation that got away. An unrecorded database is not
			// a match: it is a database that has never said what it is,
			// and treating silence as agreement is how L2 would let a
			// write through that costs rows.
			name:  "hiç kaydedilmemiş",
			state: State{},
			ahead: true,
			why:   "silence is not agreement",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Matches(); got != tc.matches {
				t.Errorf("Matches() = %v, want %v — %s", got, tc.matches, tc.why)
			}
			if got := tc.state.Ahead(); got != tc.ahead {
				t.Errorf("Ahead() = %v, want %v — %s", got, tc.ahead, tc.why)
			}
			if got := tc.state.Behind(); got != tc.behind {
				t.Errorf("Behind() = %v, want %v — %s", got, tc.behind, tc.why)
			}
		})
	}
}

// TestAheadAndBehindAreNeverBothTrue.
//
// They are two answers to one question - which side is newer - and a
// state that claims both would make whichever branch a caller wrote
// first win silently. The health page has such a chain.
func TestAheadAndBehindAreNeverBothTrue(t *testing.T) {
	for _, v := range []int{Version - 2, Version - 1, Version, Version + 1, Version + 2} {
		for _, recorded := range []bool{true, false} {
			s := State{Recorded: recorded, Version: v, Fingerprint: Fingerprint}
			if s.Ahead() && s.Behind() {
				t.Errorf("recorded=%v version=%d: both Ahead and Behind", recorded, v)
			}
		}
	}
}

// TestAMatchIsNeverAheadOrBehind.
//
// If a state can be a match *and* out of step, the page's if/else chain
// decides which sentence a customer reads by the order somebody typed
// the branches in - and that is not a decision anybody made.
func TestAMatchIsNeverAheadOrBehind(t *testing.T) {
	s := State{Recorded: true, Version: Version, Fingerprint: Fingerprint}
	if !s.Matches() {
		t.Fatal("the exact expected state does not report as a match")
	}
	if s.Ahead() || s.Behind() {
		t.Errorf("a match also reports Ahead=%v Behind=%v", s.Ahead(), s.Behind())
	}
}
