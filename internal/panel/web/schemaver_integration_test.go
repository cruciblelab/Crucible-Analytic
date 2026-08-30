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
func setSchemaRow(t *testing.T, version int, fingerprint string) {
	t.Helper()
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
	if _, err := testdb.Admin(t).Exec(context.Background(), `DELETE FROM schema_version`); err != nil {
		t.Fatalf("clearing the schema version row: %v", err)
	}
}

// TestTheHealthPageReportsTheSchemaVersion covers the four states.
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
			server, client, _ := healthServer(t)
			if tc.clear {
				clearSchemaRow(t)
			} else {
				setSchemaRow(t, tc.version, tc.fingerprint)
			}

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
	server, client, _ := healthServer(t)
	setSchemaRow(t, schemaver.Version,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

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
