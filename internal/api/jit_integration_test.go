//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// What the pool's sessions actually got, asked of PostgreSQL.
//
// # Why this is an integration test and not a check on the config struct
//
// Because the failure this guards against is silent in Go. A runtime
// parameter is a key and a string; setting one that PostgreSQL does not
// accept, or spelling the value in a way it ignores, leaves a pool that
// looks configured and behaves exactly as before. A test that read
// cfg.ConnConfig.RuntimeParams back would agree with the code and with
// nothing else - the same shape as a fixture that agrees with the
// decoder it was copied from.
//
// So the question goes to the database, through the production
// constructor, on a connection the pool handed out.
func showJIT(t *testing.T, dsn string) string {
	t.Helper()
	store, err := NewStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	var value string
	if err := store.Pool().QueryRow(context.Background(), "SHOW jit").Scan(&value); err != nil {
		t.Fatalf("SHOW jit: %v", err)
	}
	return value
}

// withParam appends one query parameter to a DSN.
//
// Built rather than concatenated with "?" because testdb.DSN is
// overridable through the environment and an override may already carry
// a query string - in which case a second "?" produces a DSN that parses
// to something nobody wrote.
func withParam(dsn, param string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + param
	}
	return dsn + "?" + param
}

// TestTheReadPoolTurnsJITOff.
//
// The reason is in NewStore's comment and the numbers are in NOTES.md:
// on this workload JIT is compile time with nothing to show for it, and
// the one endpoint where that is measurable rather than arguable is
// asns, which went from 5,689 to 3,837 seconds over 90 days with the
// two distributions disjoint.
//
// The assertion is on the session's value rather than on a duration,
// because a timing assertion in CI is a promise about a machine. This
// one is a promise about a connection.
//
// # Why the wanted value is written out and not taken from jitValue
//
// Because a mutation said so. The first version compared the database's
// answer against jitValue, and flipping that constant to "on" left this
// test green: both sides of the comparison came from the one constant
// under test, so they went on agreeing while the product changed. The
// same shape 5b hit with privacy.CanTokenise. An expectation has to come
// from somewhere the code cannot move.
func TestTheReadPoolTurnsJITOff(t *testing.T) {
	const want = "off"
	if got := showJIT(t, testdb.DSN(testdb.Reader)); got != want {
		t.Errorf("SHOW jit = %q, want %q - the pool's sessions still compile plans, "+
			"and the measurement in NOTES.md that justified this was taken with it off",
			got, want)
	}
}

// TestAnOperatorWhoNamesJITKeepsIt.
//
// The other half of the rule, and the half a test is more likely to be
// missing: NewStore sets a default, and a default that cannot be
// overridden is not a default. Two spellings reach PostgreSQL by
// different routes and both have to work, so both are asked.
//
// The second case is the one that found a defect. The first version of
// NewStore looked only at RuntimeParams["jit"] and its comment claimed
// PostgreSQL applies the startup packet's options field afterwards, so
// the documented libpq spelling would win by itself. The database said
// SHOW jit = "off": it does not, and this package was overriding an
// operator who had written the form PostgreSQL's own documentation
// shows. The claim was in a comment and nowhere else until this ran.
//
// The third case carries the other half, and without it the first two
// would pass against a NewStore that never sets anything: it asks what
// happens when the options field says something that is not about JIT.
func TestAnOperatorWhoNamesJITKeepsIt(t *testing.T) {
	base := testdb.DSN(testdb.Reader)
	for _, tc := range []struct {
		name  string
		param string
		want  string
		why   string
	}{
		{
			"as a runtime parameter", "jit=on", "on",
			"pgx parses an unrecognised key into RuntimeParams, and NewStore " +
				"only fills that key when nothing there already names JIT",
		},
		{
			"through the options field", "options=-c%20jit%3Don", "on",
			"pgx parses this into RuntimeParams[\"options\"]; PostgreSQL does " +
				"NOT let it override an individual parameter, which is why " +
				"operatorNamedJIT has to read it",
		},
		{
			// "off" written out for the reason above: an expectation taken
			// from jitValue moves with the code it is checking.
			"an options field about something else", "options=-c%20work_mem%3D64MB", "off",
			"a guard that skipped on any options field at all would leave every " +
				"deployment that tunes anything with JIT on, and the two cases " +
				"above cannot tell the difference",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := showJIT(t, withParam(base, tc.param)); got != tc.want {
				t.Errorf("SHOW jit = %q, want %q: %s", got, tc.want, tc.why)
			}
		})
	}
}
