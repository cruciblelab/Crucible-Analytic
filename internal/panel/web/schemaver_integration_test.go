//go:build integration

// The schema version, as the health page tells it.
//
// The section has four states and each says a different thing to whoever
// is looking, so each is asserted rather than one sampled. The state
// that matters is "the binary is ahead": measured against a real
// TimescaleDB, that is a collector which starts, passes its ping, and
// then loses every row it is handed - written=0, failed=3, nothing in
// the table. A page that drew that as reassuringly as a match would be
// worse than no page.
package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// setSchemaRow writes the version row as the installer would.
//
// Through the pool the test owns rather than the panel's, because
// panel_user has no INSERT on this table and is not meant to - the panel
// reporting a version it could have written itself is not a report.
//
// # It puts the row back, and that is not tidiness
//
// schema_version is one row for the whole database, shared with every
// other suite and with the next run of this one. A test that leaves a
// deliberately wrong fingerprint in it has not finished; it has moved
// its own subject matter into somebody else's test.
//
// Measured: TestASchemaWhoseNumberAgreesAndFingerprintDoesNotIsAMismatch
// left "deadbeef..." behind, and the *next* run of this package failed in
// health_integration_test.go - a different file, a different claim, and a
// failure that reproduced only in a full run. Restoring here rather than
// in each caller means nobody has to remember.
//
// # And it takes the lock, for the same reason
//
// The lock used to be taken by one test in this file, and the other test
// that writes this row did not take it. Nothing noticed: the invariant
// that guards the row asked whether the *package* mentioned the lock,
// and it did. That test wrote the row with the right version and a
// wrong fingerprint, then put back whatever it had read - and what it
// had read could be internal/panel's "behind" state or the real one, in
// the middle of internal/panel's own test. CI 420 failed on exactly
// that: internal/panel set the row behind, checked the status, and was
// told "the schema is already the one this build expects" one call
// later. Run together on purpose, the two left the shared row at 23 in
// three runs out of three.
//
// Here, the only way to write the row is through a helper that holds the
// lock first. Before its cleanup is registered, too: cleanups run last
// in, first out, so the row is put back while the lock is still held.
//
// Call it before anything that takes testdb.AccountsLock (healthServer
// does). Every suite that holds both takes this one first, and two
// suites taking a pair in opposite orders deadlock.
//
// Once per test: testdb.Lock is not re-entrant, and says so.
func restoreSchemaRow(t *testing.T) {
	t.Helper()
	testdb.Lock(t, testdb.Admin(t), testdb.SchemaVersionLock)
	admin := testdb.Admin(t)
	var (
		version     int
		fingerprint string
		by          string
	)
	err := admin.QueryRow(context.Background(),
		`SELECT version, fingerprint, applied_by FROM schema_version WHERE id = 1`).
		Scan(&version, &fingerprint, &by)
	had := err == nil
	t.Cleanup(func() {
		bg := context.Background()
		if !had {
			_, _ = admin.Exec(bg, `DELETE FROM schema_version WHERE id = 1`)
			return
		}
		_, _ = admin.Exec(bg, `
			INSERT INTO schema_version (id, version, fingerprint, applied_by)
			VALUES (1, $1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET
			    version = EXCLUDED.version,
			    fingerprint = EXCLUDED.fingerprint,
			    applied_by = EXCLUDED.applied_by`, version, fingerprint, by)
	})
}

func setSchemaRow(t *testing.T, version int, fingerprint string) {
	t.Helper()
	restoreSchemaRow(t)
	admin := testdb.Admin(t)
	_, err := admin.Exec(context.Background(), `
		INSERT INTO schema_version (id, version, fingerprint, applied_by)
		VALUES (1, $1, $2, 'test')
		ON CONFLICT (id) DO UPDATE SET
		    version = EXCLUDED.version,
		    fingerprint = EXCLUDED.fingerprint,
		    applied_by = EXCLUDED.applied_by`, version, fingerprint)
	if err != nil {
		t.Fatalf("writing the schema version row: %v", err)
	}
}

func clearSchemaRow(t *testing.T) {
	t.Helper()
	restoreSchemaRow(t)
	if _, err := testdb.Admin(t).Exec(context.Background(), `DELETE FROM schema_version`); err != nil {
		t.Fatalf("clearing the schema version row: %v", err)
	}
}

// TestTheHealthPageReportsTheSchemaVersion covers the four states.
//
// schema_version is one row for the whole database, and this test spends
// its time putting it into states and asking what the page says. Another
// suite writing it from another process made this pass alone and fail in
// the full run, reporting a page in "another state" - which it was, just
// not one this test had set. Each case holds the lock through
// setSchemaRow or clearSchemaRow, which is the only way this file writes
// the row.
func TestTheHealthPageReportsTheSchemaVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		version     int
		fingerprint string
		clear       bool
		want        string
		// notWant is a list, not one string. Measured why: a mutation
		// that made an unrecorded database report as a match survived a
		// single-string version of this test, because the case asserted
		// only that the *warning* was absent - and it was. The sentence
		// that must not appear in three of these four states is the
		// reassuring one, and nothing was asking about it.
		notWant []string
	}{
		{
			name:        "uyuşuyor",
			version:     schemaver.Version,
			fingerprint: schemaver.Fingerprint,
			want:        "bu yapının beklediğiyle aynı",
			notWant:     []string{"satırları kaybeder", "geri almış", "kaydedilmeye başlanmadan"},
		},
		{
			// The one that costs data. The sentence has to name the
			// consequence, because "schema mismatch" is not a thing a
			// customer can act on and "your collector is losing rows"
			// is.
			name:        "binary ileride",
			version:     schemaver.Version - 1,
			fingerprint: "0000000000000000000000000000000000000000000000000000000000000000",
			want:        "satırları kaybeder",
			notWant:     []string{"beklediğiyle aynı", "geri almış"},
		},
		{
			name:        "veritabanı ileride",
			version:     schemaver.Version + 1,
			fingerprint: "1111111111111111111111111111111111111111111111111111111111111111",
			want:        "geri almış",
			notWant:     []string{"beklediğiyle aynı", "satırları kaybeder"},
		},
		{
			name:  "hiç kaydedilmemiş",
			clear: true,
			want:  "kaydedilmeye başlanmadan önce kurulmuş",
			// "beklediğiyle aynı" first: a database that has never
			// recorded a version is not a match, and calling it one
			// sends the reader away from the only screen that could
			// have told them otherwise.
			notWant: []string{"beklediğiyle aynı", "satırları kaybeder", "geri almış"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The row first: it takes SchemaVersionLock, and that comes
			// before the AccountsLock healthServer takes.
			if tc.clear {
				clearSchemaRow(t)
			} else {
				setSchemaRow(t, tc.version, tc.fingerprint)
			}
			server, client, _ := healthServer(t)

			status, body := get(t, client, server.URL+HealthPath)
			if status != http.StatusOK {
				t.Fatalf("the health page answered %d", status)
			}
			if !strings.Contains(body, "Şema sürümü") {
				t.Fatal("the page has no schema section at all")
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("the page does not say %q", tc.want)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(body, bad) {
					t.Errorf("the page says %q, which belongs to another state", bad)
				}
			}
		})
	}
}

// TestASchemaWhoseNumberAgreesAndFingerprintDoesNotIsAMismatch.
//
// The case a version-only check cannot see, and the reason the
// fingerprint exists: same number, different schema. It is what a
// half-applied upgrade leaves behind - somebody ran the migration, it
// failed partway, and the row still says what it said before.
//
// The page must not call that a match. A green line here would send the
// person looking at it away from the one screen that could have told
// them.
func TestASchemaWhoseNumberAgreesAndFingerprintDoesNotIsAMismatch(t *testing.T) {
	setSchemaRow(t, schemaver.Version,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	server, client, _ := healthServer(t)

	status, body := get(t, client, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}
	if strings.Contains(body, "bu yapının beklediğiyle aynı") {
		t.Error("the page called it a match; the version agrees but the schema is not the same one")
	}
	if !strings.Contains(body, "satırları kaybeder") {
		t.Error("the page does not warn, even though the installed schema is not the expected one")
	}
}

// TestThePanelCannotWriteTheSchemaVersion.
//
// The grant, asserted from the panel's own connection rather than from
// grants.sql - a GRANT that ran without error proves the statement was
// accepted, not that the privilege is absent.
//
// It matters because the health page's whole claim is that the version
// comes from outside the process reporting it. A panel that could write
// this row would be quoting itself.
func TestThePanelCannotWriteTheSchemaVersion(t *testing.T) {
	// Held and put back although the write is meant to fail: if the grant
	// ever regresses, this test fails alone instead of leaving version
	// 999 in the row for every other suite to trip on.
	restoreSchemaRow(t)
	_, _, store := healthServer(t)

	_, err := store.Pool().Exec(context.Background(),
		`UPDATE schema_version SET version = 999 WHERE id = 1`)
	if err == nil {
		t.Fatal("the panel updated schema_version; it is supposed to have SELECT and nothing else")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the write failed, but not for want of privilege: %v", err)
	}
}
