//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// W2, through the real read API and real rows: the dashboard leaves the
// automated events out of its figures and says how many it left out. And
// on a site where nothing was automated, it says nothing - a sentence
// that is always there would be a sentence nobody reads.
func TestTheDashboardSaysHowManyEventsItLeftOutAsAutomated(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	base, token := analyticsAPI(t)
	client, err := analytics.New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	const withBots, clean = "w2-pano-otomatik", "w2-pano-temiz"
	owner := makeUser(t, store, "w2-pano-sahip", false)
	for _, site := range []string{withBots, clean} {
		if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	when := time.Now().Add(-2 * time.Hour)
	seedBeacon(t, withBots, when, []beaconRow{
		{visitor: "insan", kind: "pageview", path: "/"},
		{visitor: "insan", kind: "pageview", path: "/fiyat"},
		{visitor: "curl", kind: "pageview", path: "/", autoReason: "collector"},
		{visitor: "curl", kind: "pageview", path: "/", autoReason: "collector"},
		{visitor: "curl", kind: "pageview", path: "/", autoReason: "collector"},
		{visitor: "sahte", kind: "pageview", path: "/", autoReason: "tampered"},
	})
	seedBeacon(t, clean, when, []beaconRow{{visitor: "insan", kind: "pageview", path: "/"}})

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)
	lang := srv.Renderer.Catalogs().Base()
	sentence, _, _ := strings.Cut(lang.T("pano.otomatik_dislanan"), "%s")
	after := strings.SplitN(lang.T("pano.otomatik_dislanan"), "%s", 2)[1]
	head, _, _ := strings.Cut(after, ":")

	status, body := get(t, c, server.URL+sitePath(withBots))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}
	if !strings.Contains(body, shown(sentence+"4"+head)) {
		t.Errorf("the dashboard does not say 4 events were left out as automated")
	}

	_, body = get(t, c, server.URL+sitePath(clean))
	if strings.Contains(body, shown(head)) {
		t.Error("a site with nothing automated still carries the sentence")
	}
}
