//go:build integration

// One VDS, three customers, and nothing leaking between them.
//
// The requirement in the customer's own words: *"Tek VDS'te 3 farklı
// müşteri 3 farklı web sitesi olabilir ama hepsi ayrı kendi içinde
// olacak."* Today that separation rests on one thing - the panel's own
// membership check - because the API token the panel holds reads every
// site on the machine. There is no database boundary underneath it and
// there is not meant to be: `panel_site_members` is the boundary.
//
// A boundary that rests on one check needs a test per door.
//
// # The thing these tests are actually about
//
// Not "an outsider cannot read the numbers" - that was already tested
// for two of the five site-scoped routes. The sharper claim, and the one
// nothing measured, is:
//
//	an outsider cannot tell a site that exists from one that does not.
//
// Two handlers already carry a comment saying they return 404 rather
// than 403 for exactly this reason. Neither comment was checked. A 403,
// a different body, a different length, one word - any of them turns a
// URL into a way to enumerate every customer on the machine from any
// account on it, including a trial account somebody was given for an
// afternoon.
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

const (
	// theirSite is a real site the outsider is not a member of.
	theirSite = "yalitim-onlarin"
	// noSuchSite is a site id nobody has ever created. The point of the
	// pair is that the answers have to be identical.
	noSuchSite = "yalitim-hic-yok"
	// ourSite is the outsider's own, so the tests can tell "refused
	// everything" from "refused the right thing".
	ourSite = "yalitim-bizim"
)

// siteScopedRoutes is every route that takes a {site} and the URL that
// reaches it.
//
// The list is the "against a list, not against memory" half of this
// file. TestEverySiteScopedRouteIsOnThisList reads the router and fails
// when the two disagree - so a new per-site page cannot be added
// without somebody meeting this test.
var siteScopedRoutes = map[string]func(site string) string{
	"dashboard": func(site string) string { return MembersPathPrefix + site },
	"members":   func(site string) string { return MembersPathPrefix + site + membersPathSuffix },
	"breakdown": func(site string) string {
		return MembersPathPrefix + site + breakdownPathSegment + string(analytics.BreakdownPages)
	},
	"addressList": func(site string) string {
		return MembersPathPrefix + site + addressListPathSegment + "sessiz"
	},
}

// isolationServer signs in a user who owns ourSite and nothing else,
// with theirSite existing and belonging to somebody else.
func isolationServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()

	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()

	outsider := makeUser(t, store, "yalitim-yabanci", false)
	if err := store.AddMember(ctx, ourSite, outsider.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	// theirSite has an owner who is not our user. Without this the site
	// would have no members at all, which is a third state and not the
	// one being tested.
	neighbour := makeUser(t, store, "yalitim-komsu", false)
	if err := store.AddMember(ctx, theirSite, neighbour.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, outsider.Email)
}

// TestAnOutsiderCannotTellAnExistingSiteFromOneThatDoesNotExist.
//
// The headline. For every site-scoped route, the answer for a site that
// exists and the answer for a site that does not have to be the same
// bytes.
//
// Body equality, not just status equality. Two 404s that differ by a
// single word are still an oracle, and this project has already found
// one page whose "not found" and "no access" wording differed by more
// than a word.
func TestAnOutsiderCannotTellAnExistingSiteFromOneThatDoesNotExist(t *testing.T) {
	server, client := isolationServer(t)

	for name, path := range siteScopedRoutes {
		t.Run(name, func(t *testing.T) {
			existsStatus, existsBody := get(t, client, server.URL+path(theirSite))
			absentStatus, absentBody := get(t, client, server.URL+path(noSuchSite))

			if existsStatus != http.StatusNotFound {
				t.Errorf("a site that exists but is not ours answered %d; want 404, "+
					"because a 403 confirms the site is there", existsStatus)
			}
			if existsStatus != absentStatus {
				t.Errorf("status differs: %d for a site that exists, %d for one that does not - "+
					"the difference is enough to enumerate every customer on this machine",
					existsStatus, absentStatus)
			}
			if normalise(existsBody, theirSite, noSuchSite) != normalise(absentBody, theirSite, noSuchSite) {
				t.Errorf("the two answers differ in their body; %d bytes vs %d. "+
					"A page that says a different thing about a site that exists is an oracle",
					len(existsBody), len(absentBody))
			}
			if strings.Contains(existsBody, theirSite) {
				t.Errorf("the refusal page names %s, which is the one thing it must not confirm", theirSite)
			}
		})
	}
}

// normalise removes the parts of a page that legitimately differ
// between two requests, so the comparison is about what the page says.
//
// The site id itself is one of them: an error page that echoes the id
// back is not leaking anything the requester did not already type. The
// CSRF token is another - a fresh one per render, and identical pages
// would otherwise never compare equal.
func normalise(body, existing, absent string) string {
	body = strings.ReplaceAll(body, existing, "<SITE>")
	body = strings.ReplaceAll(body, absent, "<SITE>")
	body = csrfValue.ReplaceAllString(body, `value="<CSRF>"`)
	return body
}

var csrfValue = regexp.MustCompile(`value="[A-Za-z0-9_\-]{20,}"`)

// TestTheSiteSelectorNamesOnlyOurOwnSites.
//
// The first page after signing in lists what the account can reach. A
// selector built from "every site in the database" rather than "every
// site this account is a member of" would hand over the customer list
// on the first screen, and it would look right to whoever wrote it
// because their own account can see everything.
func TestTheSiteSelectorNamesOnlyOurOwnSites(t *testing.T) {
	server, client := isolationServer(t)

	status, body := get(t, client, server.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("the site selector answered %d", status)
	}
	if !strings.Contains(body, ourSite) {
		t.Errorf("the selector does not list %s, which this account owns", ourSite)
	}
	if strings.Contains(body, theirSite) {
		t.Errorf("the selector names %s, which belongs to another customer", theirSite)
	}
}

// TestEverySiteScopedRouteIsOnThisList is the two-way mirror.
//
// Read out of the router rather than remembered: a new per-site page is
// a new door, and the failure it causes when nobody tests it is not a
// crash - it is one customer reading another's numbers, indefinitely,
// with nothing in any log.
//
// The router's own registrations are the source of truth, so this
// cannot be satisfied by editing the list alone.
func TestEverySiteScopedRouteIsOnThisList(t *testing.T) {
	root := repoRootForIsolation(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "panel", "web", "server.go"))
	if err != nil {
		t.Fatal(err)
	}

	// Registrations of the form mux.HandleFunc(MembersPathPrefix+"{site}"+...)
	registered := regexp.MustCompile(`mux\.HandleFunc\(MembersPathPrefix\+"\{site\}"([^,]*),\s*s\.(\w+)\)`)
	matches := registered.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		t.Fatal("no site-scoped routes found in server.go; this test would pass by comparing nothing")
	}
	if len(matches) != len(siteScopedRoutes) {
		var handlers []string
		for _, m := range matches {
			handlers = append(handlers, m[2])
		}
		t.Errorf("the router registers %d site-scoped routes (%s) and this file tests %d. "+
			"Every per-site page is a door between two customers; add it to siteScopedRoutes "+
			"and let the tests above run against it",
			len(matches), strings.Join(handlers, ", "), len(siteScopedRoutes))
	}
}

func repoRootForIsolation(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/panel/web -> repository root
	return filepath.Dir(filepath.Dir(filepath.Dir(wd)))
}

// TestTheAuditLogIsReadSiteScoped.
//
// There is no audit page yet - CapViewAudit exists and nothing routes to
// it - so this tests the store the page will use, which is where the
// mistake would be made. An unscoped read is the right behaviour for an
// operator at a shell and the wrong one for anything a customer's
// browser reaches, and the two are one argument apart.
func TestTheAuditLogIsReadSiteScoped(t *testing.T) {
	_, store := setupTestServer(t)
	ctx := context.Background()

	actor := makeUser(t, store, "yalitim-denetim", false)
	if err := store.AddMember(ctx, ourSite, actor.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	for _, site := range []string{ourSite, theirSite} {
		if err := store.Record(ctx, panel.AuditEntry{
			ActorKind:  panel.PrincipalUser,
			ActorID:    &actor.ID,
			ActorLabel: actor.Email,
			Action:     panel.ActionTechnicalDoorOpened,
			SiteID:     site,
		}); err != nil {
			t.Fatalf("recording an entry for %s: %v", site, err)
		}
	}

	entries, _, err := store.Audit(ctx, panel.AuditFilter{Sites: []string{ourSite}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("a site-scoped audit read returned nothing; the fixture did not land")
	}
	for _, e := range entries {
		if e.SiteID != ourSite {
			t.Errorf("a read scoped to %s returned an entry for %s", ourSite, e.SiteID)
		}
	}

	// And the unscoped read does see both, so the assertion above is
	// about the scoping rather than about an empty table.
	all, _, err := store.Audit(ctx, panel.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sites := map[string]bool{}
	for _, e := range all {
		sites[e.SiteID] = true
	}
	if !sites[ourSite] || !sites[theirSite] {
		t.Errorf("an unscoped read saw %v; both sites should be there, or the scoped test above proves nothing", sites)
	}
}
