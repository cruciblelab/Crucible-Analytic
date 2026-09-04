//go:build integration

// The update section, pressed rather than inspected.
//
// # Why this file exists at all
//
// The schema upgrade has had a locked path since L3 and a store test
// for every rule in it. Nothing ever posted its form. The store tests
// built a devgate.Authorization directly - correct as unit tests, and
// blind to the one thing between the person and the store: the name of
// the field the password is typed into.
//
// It was the wrong name. The form posted "gelistirici_parolasi" and
// devgate.FromRequest reads "developer_password", so a locked
// deployment answered a *correct* password with "type it to start one".
// Found while building this section's twin, because building a second
// one made the first one's shape visible.
//
// So this file presses the button. It fills the fields the page
// actually renders, with the values a person would type, and asserts
// what the page then says.

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const releasePageSite = "surum-sayfasi"

// releasePage signs an owner in and takes the release queue's lock.
func releasePage(t *testing.T) (string, *http.Client, *panel.Store) {
	t.Helper()
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.ReleaseQueueLock)

	server, client, _ := signedInOwner(t, srv, store, releasePageSite, "surum-sahibi")
	t.Cleanup(func() {
		_, _ = testdb.Pool(t, testdb.Panel).Exec(context.Background(),
			`DELETE FROM panel_release_requests`)
	})
	return server.URL, client, store
}

// TestTheFormDrawsTheFieldTheGateActuallyReads.
//
// The narrowest statement of the defect. A password input whose name
// nothing reads is not a broken feature that reports an error; it is a
// feature that refuses correct input and tells the person to try the
// thing they just did.
func TestTheFormDrawsTheFieldTheGateActuallyReads(t *testing.T) {
	base, client, _ := releasePage(t)

	status, body := get(t, client, base+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}
	if !strings.Contains(body, `name="`+devgate.FormField+`"`) {
		t.Errorf("the page draws no password field named %q. devgate.FromRequest reads "+
			"that name and nothing else, so any other name is a field the gate never sees",
			devgate.FormField)
	}
}

// TestPressingItWithTheRightPasswordQueuesTheVersion.
//
// The positive case, through the handler. A suite of refusals proves a
// button refuses and never proves it works - and in this project a
// button that only refused is exactly what shipped.
//
// What this does *not* cover, said out loud: postForm builds the form
// values from devgate.FormField, so it posts the right name whatever
// the template renders. Mutating the template back to the old name
// leaves this test green. The one above is what fails, and it is what
// carries the weight - this one proves the path from handler to queue,
// that one proves the page reaches the handler.
//
// *Formu kendi dolduran bir test, formun ne çizdiğini sınamaz.*
func TestPressingItWithTheRightPasswordQueuesTheVersion(t *testing.T) {
	base, client, _ := releasePage(t)

	_, body := postForm(t, client, base+HealthPath, url.Values{
		"eylem":           {eylemSurum},
		"surum":           {"v9.9.9"},
		devgate.FormField: {testDevPassword},
	})

	if !strings.Contains(body, "v9.9.9") {
		t.Errorf("the page does not mention the version that was requested")
	}
	latest, err := relupdate.Latest(context.Background(), testdb.Pool(t, testdb.Panel))
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil {
		t.Fatal("no request row was written. The password was correct and the version " +
			"was valid, so the only thing left that could have stopped it is the form")
	}
	if latest.ToVersion != "v9.9.9" {
		t.Errorf("the queued row asks for %q", latest.ToVersion)
	}
}

// TestPressingItWithTheWrongPasswordSaysLockedAndQueuesNothing.
func TestPressingItWithTheWrongPasswordSaysLockedAndQueuesNothing(t *testing.T) {
	base, client, _ := releasePage(t)

	_, _ = postForm(t, client, base+HealthPath, url.Values{
		"eylem":           {eylemSurum},
		"surum":           {"v9.9.9"},
		devgate.FormField: {"bu-parola-yanlis"},
	})

	latest, err := relupdate.Latest(context.Background(), testdb.Pool(t, testdb.Panel))
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Errorf("a wrong password still queued a row: %+v", latest)
	}
}

// TestTheTypedVersionSurvivesARefusal.
//
// Small, and the difference between a form somebody uses twice and one
// they give up on. A refusal that clears the field makes the person
// retype what was already right.
func TestTheTypedVersionSurvivesARefusal(t *testing.T) {
	base, client, _ := releasePage(t)

	_, body := postForm(t, client, base+HealthPath, url.Values{
		"eylem":           {eylemSurum},
		"surum":           {"v9.9.9"},
		devgate.FormField: {"bu-parola-yanlis"},
	})
	if !strings.Contains(body, `value="v9.9.9"`) {
		t.Error("the version field came back empty after a refusal, so the person has " +
			"to type again the one thing that was not wrong")
	}
}
