//go:build integration

// Three forms put something that was not a site into an operation
// record's site column.
//
// The release form put the version the person typed there, and it is
// read before anything checks it. site_id is a key of
// idx_panel_operations_site; a typed string longer than PostgreSQL's
// 2704-byte B-tree entry made the INSERT that *was* the record fail, and
// the refusal that followed was written to the log and nowhere else.
// Measured on the real table before the fix: 3200 bytes of distinct
// four-byte characters, "index row size 3224 exceeds btree version 4
// maximum 2704". The two backup buttons put the backup's number there:
// short enough to fit, and a record saying the check happened to a site
// called "17".
//
// These press the forms, because the fault was in which argument a
// handler passed, and a store test that calls BeginOperation directly
// passes whichever argument the test chooses.

package web

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// operationSince reads the newest operation of one action started after
// a moment, with the two columns this file is about.
//
// By action and time rather than by target, because the target is the
// thing under test: looking a row up by the value it should hold finds
// nothing when it holds something else, and "no row" is the failure
// the release form had - the two have to be told apart.
func operationSince(t *testing.T, action string, since time.Time) (target, site, outcome string, found bool) {
	t.Helper()
	err := testdb.Pool(t, testdb.Panel).QueryRow(context.Background(), `
		SELECT target, site_id, outcome FROM panel_operations
		WHERE action = $1 AND started_at >= $2
		ORDER BY started_at DESC LIMIT 1`, action, since).
		Scan(&target, &site, &outcome)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", "", "", false
		}
		t.Fatalf("reading the %s operation: %v", action, err)
	}
	return target, site, outcome, true
}

// overLongVersion is a typed version no index entry can hold.
//
// Distinct four-byte characters, not one repeated: PostgreSQL compresses
// a long value before measuring it, and a repeated character compresses
// below the limit and hides it (H7, NOTES "İndeks girdisi sınırı").
func overLongVersion() string {
	r := rand.New(rand.NewSource(7))
	var b strings.Builder
	for b.Len() < 3200 {
		b.WriteRune(rune(0x20000 + r.Intn(0x2A6DF-0x20000)))
	}
	return b.String()
}

// dbNow is the database's clock, so "started after" compares one clock
// with itself.
func dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := testdb.Pool(t, testdb.Panel).QueryRow(context.Background(),
		`SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// TestAnOverLongVersionIsStillRecordedAsARefusal.
//
// The case the record exists for: something was attempted and refused.
// The correct password, so the request reaches the version check and is
// refused there - a wrong one would be refused by the gate and prove
// the same thing less, since the record opens before either.
func TestAnOverLongVersionIsStillRecordedAsARefusal(t *testing.T) {
	base, client, _ := releasePage(t)
	typed := overLongVersion()
	if len(typed) <= 2704 {
		t.Fatalf("the typed version is %d bytes; it has to be past the index limit "+
			"or this test asks nothing", len(typed))
	}

	since := dbNow(t)
	_, _ = postForm(t, client, base+HealthPath, url.Values{
		"eylem":           {eylemSurum},
		"surum":           {typed},
		devgate.FormField: {testDevPassword},
	})

	target, site, outcome, found := operationSince(t, panel.ActionReleaseRequested, since)
	if !found {
		t.Fatal("no operation was recorded for the release request. The record opens " +
			"before the version is checked, so whatever the person typed reaches it - " +
			"and a typed value too long for an index entry cost the record")
	}
	if site != "" {
		t.Errorf("the release operation's site is %d bytes of what was typed; a release "+
			"happens to the installation, not to a site", len(site))
	}
	if outcome != string(panel.OutcomeRefused) {
		t.Errorf("the outcome is %q, want %q - the version was refused", outcome, panel.OutcomeRefused)
	}
	if !strings.HasPrefix(typed, target) || target == "" {
		t.Errorf("the target is not the start of what was typed (%d bytes)", len(target))
	}
	if len(target) > 256 || !utf8.ValidString(target) {
		t.Errorf("the target is %d bytes (valid UTF-8: %v); it should be cut to 256 "+
			"bytes and never inside a character", len(target), utf8.ValidString(target))
	}
}

// TestAVersionWithAControlCharacterIsStillRecorded.
//
// The other way a typed string costs an INSERT: PostgreSQL refuses a
// NUL in text outright, whatever the column or its index.
func TestAVersionWithAControlCharacterIsStillRecorded(t *testing.T) {
	base, client, _ := releasePage(t)

	since := dbNow(t)
	_, _ = postForm(t, client, base+HealthPath, url.Values{
		"eylem":           {eylemSurum},
		"surum":           {"v1.2\x00.3"},
		devgate.FormField: {testDevPassword},
	})

	target, _, _, found := operationSince(t, panel.ActionReleaseRequested, since)
	if !found {
		t.Fatal("no operation was recorded for a version with a NUL in it")
	}
	if target != "v1.2.3" {
		t.Errorf("the target is %q, want the typed version without its control character", target)
	}
}

// backupPage signs an owner in with the backup queue to itself.
//
// BackupQueueLock before setupTestServer's AccountsLock, the order
// testdb declares them in. The queue is swept at both ends through the
// panel's role, the one its DELETE policy names: neither button needs a
// password - a backup check is the customer's own question - so pressing
// either writes a request, and a request left pending is the one
// in-flight slot every other suite's Ask then finds taken. (The first
// version of this test assumed a wrong password stopped them, and left
// exactly such a row behind.)
func backupPage(t *testing.T) (string, *http.Client) {
	t.Helper()
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.BackupQueueLock)
	sweep := func() {
		if _, err := testdb.Pool(t, testdb.Panel).Exec(context.Background(),
			`DELETE FROM panel_backup_requests`); err != nil {
			t.Errorf("sweeping the backup queue: %v", err)
		}
	}
	sweep()
	t.Cleanup(sweep)

	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	server, client, _ := signedInOwner(t, srv, store, releasePageSite, "yedek-sahibi")
	return server.URL, client
}

// TestTheBackupButtonsNameTheBackupAndNoSite.
//
// The check first, which writes its request and succeeds; then the
// restore while the check is still pending, which the queue refuses.
// Both records name the backup in the audit log's words and no site,
// and the second is recorded as what it is - a refusal.
func TestTheBackupButtonsNameTheBackupAndNoSite(t *testing.T) {
	base, client := backupPage(t)
	const id = 987654321

	for _, c := range []struct {
		eylem, action string
		outcome       panel.Outcome
	}{
		{eylemYedekDogrula, panel.ActionBackupVerified, panel.OutcomeSucceeded},
		{eylemYedekYukle, panel.ActionBackupRestored, panel.OutcomeRefused},
	} {
		since := dbNow(t)
		_, _ = postForm(t, client, base+HealthPath, url.Values{
			"eylem": {c.eylem},
			"yedek": {fmt.Sprint(id)},
		})
		target, site, outcome, found := operationSince(t, c.action, since)
		if !found {
			t.Errorf("%s: no operation was recorded", c.eylem)
			continue
		}
		if site != "" {
			t.Errorf("%s: the operation's site is %q - a backup is not a site", c.eylem, site)
		}
		if outcome != string(c.outcome) {
			t.Errorf("%s: recorded as %q, want %q", c.eylem, outcome, c.outcome)
		}
		if want := fmt.Sprintf("yedek-%d", id); target != want {
			t.Errorf("%s: the target is %q, want %q, the audit log's name for it", c.eylem, target, want)
		}
	}
}

// TestAWrongPasswordOnTheReleaseFormIsARefusal.
//
// The release form's refusals were not on outcomeFor's list, so every
// wrong password on it was recorded as a failure - in a table whose
// schema says a refusal is its own outcome because burying refusals
// among failures is how a real fault gets missed.
func TestAWrongPasswordOnTheReleaseFormIsARefusal(t *testing.T) {
	base, client, _ := releasePage(t)

	since := dbNow(t)
	_, _ = postForm(t, client, base+HealthPath, url.Values{
		"eylem":           {eylemSurum},
		"surum":           {"v9.9.9"},
		devgate.FormField: {"bu-parola-yanlis"},
	})
	target, _, outcome, found := operationSince(t, panel.ActionReleaseRequested, since)
	if !found {
		t.Fatal("no operation was recorded for the release request")
	}
	if target != "v9.9.9" {
		t.Errorf("the target is %q, want the version that was typed", target)
	}
	if outcome != string(panel.OutcomeRefused) {
		t.Errorf("a wrong password is recorded as %q, want %q", outcome, panel.OutcomeRefused)
	}
}
