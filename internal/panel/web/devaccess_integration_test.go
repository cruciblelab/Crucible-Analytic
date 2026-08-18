//go:build integration

// The owner's side of developer access, against a real database.
//
// The unit tests decide how a row is drawn. What only a live run can
// prove is the sequence the mechanism exists for: somebody with a shell
// asks, the owner is shown it, the owner decides, and the link then does
// or does not open - with the decision recorded under the name of the
// person who made it.

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// requestAccess mints a developer request the way the shell command
// does, tagged so the suite's cleanup finds it.
func requestAccess(t *testing.T, store *panel.Store, why string) (string, panel.DevAccessRequest) {
	t.Helper()
	token, req, err := store.RequestDevAccess(context.Background(),
		testReasonPrefix+why, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	return token, req
}

// TestTheOwnerDecidesAndTheLinkObeys is the whole flow, both ways.
func TestTheOwnerDecidesAndTheLinkObeys(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "erisim-testi"

	owner := makeUser(t, store, "erisim-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + DevAccessRequestsPath

	approveToken, approveReq := requestAccess(t, store, "onaylanacak")
	denyToken, denyReq := requestAccess(t, store, "reddedilecek")

	client := signedIn(t, server.URL, owner.Email)

	// ---- both requests are on the page, with their reasons ----
	status, body := get(t, client, page)
	if status != http.StatusOK {
		t.Fatalf("the owner got %d from the approval page", status)
	}
	for _, want := range []string{approveReq.Reason, denyReq.Reason} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show the reason %q", want)
		}
	}
	// The sentence that says the panel verified nothing about who asked.
	// Without it the reason reads as an identity, which is the one thing
	// this page must not let happen.
	if !strings.Contains(body, srv.Renderer.Catalogs().Base().T("erisim.kim")) {
		t.Error("the page does not say the requester is unverified")
	}

	// ---- approve one ----
	status, body = post(t, client, page, url.Values{
		"islem": {"onayla"}, "istek": {strconv.FormatInt(approveReq.ID, 10)},
	})
	if status != http.StatusOK {
		t.Fatalf("approving answered %d: %s", status, messageOf(body))
	}

	// ---- deny the other ----
	status, body = post(t, client, page, url.Values{
		"islem": {"reddet"}, "istek": {strconv.FormatInt(denyReq.ID, 10)},
	})
	if status != http.StatusOK {
		t.Fatalf("denying answered %d: %s", status, messageOf(body))
	}

	// ---- the approved link now opens ----
	if _, err := store.RedeemDevAccess(ctx, approveToken, netip.Addr{}); err != nil {
		t.Fatalf("the approved link was refused: %v", err)
	}
	// ---- and the denied one never will ----
	if _, err := store.RedeemDevAccess(ctx, denyToken, netip.Addr{}); err == nil {
		t.Fatal("the denied link opened a session")
	}

	// ---- the decisions are in the audit log, under the owner's name ----
	entries, _, err := store.Audit(ctx, panel.AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	want := map[string]string{
		panel.ActionDevAccessApproved: "dev_access:" + strconv.FormatInt(approveReq.ID, 10),
		panel.ActionDevAccessDenied:   "dev_access:" + strconv.FormatInt(denyReq.ID, 10),
		// Written by RequestDevAccess. Without it the record begins at
		// "somebody approved something", which reads as though the panel
		// invented the request.
		panel.ActionDevAccessRequested: "dev_access:" + strconv.FormatInt(approveReq.ID, 10),
	}
	found := map[string]bool{}
	for _, e := range entries {
		target, ok := want[e.Action]
		if !ok || e.Target != target {
			continue
		}
		found[e.Action] = true
		switch e.Action {
		case panel.ActionDevAccessApproved, panel.ActionDevAccessDenied:
			if e.ActorLabel != owner.Email {
				t.Errorf("%s is filed under %q, not the owner who clicked", e.Action, e.ActorLabel)
			}
			if e.ActorID == nil || *e.ActorID != owner.ID {
				t.Errorf("%s carries no usable actor id", e.Action)
			}
		case panel.ActionDevAccessRequested:
			// Nobody named, on purpose: whoever ran the command had a
			// shell, and that is the whole of what is known.
			if e.ActorKind != panel.PrincipalDeveloper {
				t.Errorf("the request is filed under %q, not the developer", e.ActorKind)
			}
		}
	}
	for action := range want {
		if !found[action] {
			t.Errorf("no audit entry for %s", action)
		}
	}
}

// TestADeveloperCannotApproveDeveloperAccess.
//
// A redeemed link produces a principal with Superadmin set, because a
// developer has to reach every site to do the work. ownsAnySite
// therefore answers yes for them, and a page that asked only that would
// let an approved developer approve the next request, and the next, and
// the owner would be asked exactly once - ever.
//
// This test was run against requireDecider with its Kind check removed,
// which is the honest way to find out what it proves. The answer is
// worth writing down: the developer is still refused, because the next
// line loads a User by an id a developer does not have and the load
// fails - but they are refused with a **500**, by an accident nobody
// designed. So the assertion here is not "the request failed"; it is
// that the status is 403. A rule whose only enforcement is a lookup
// failing elsewhere is a rule that ends the day somebody fixes that
// lookup.
//
// The forged POST carries a valid CSRF token, taken from a page the
// developer legitimately sees, so it fails on authority rather than on a
// missing token.
func TestADeveloperCannotApproveDeveloperAccess(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "erisim-gelistirici"

	owner := makeUser(t, store, "erisim-sahip2", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	// A developer session, obtained the only way one can be: an approved
	// link, redeemed.
	liveToken, liveReq := requestAccess(t, store, "gecerli-oturum")
	if err := store.ApproveDevAccess(ctx, liveReq.ID, owner); err != nil {
		t.Fatalf("ApproveDevAccess: %v", err)
	}
	// Redeeming answers 303 to the wizard, and this client follows
	// nothing - the redirect is the success signal, not a step on the
	// way to one.
	dev := newClient(t, server.URL)
	status, _ := get(t, dev, server.URL+DevAccessPathPrefix+liveToken)
	if status != http.StatusSeeOther {
		t.Fatalf("redeeming the developer link answered %d, want 303", status)
	}

	// The request this developer would like to approve for themselves.
	nextToken, nextReq := requestAccess(t, store, "kendine-onay")

	// The page itself is refused.
	status, _ = get(t, dev, server.URL+DevAccessRequestsPath)
	if status != http.StatusForbidden {
		t.Errorf("the approval page answered %d to a developer, want 403", status)
	}

	// And so is the POST, carrying a token the developer really holds -
	// taken from the setup wizard, which is their page.
	_, wizard := get(t, dev, server.URL+SetupPathPrefix+wizardSteps[0].ID)
	token := csrfFrom(t, wizard)
	status, _ = postWithToken(t, dev, server.URL+DevAccessRequestsPath, token, url.Values{
		"islem": {"onayla"}, "istek": {strconv.FormatInt(nextReq.ID, 10)},
	})
	if status != http.StatusForbidden {
		t.Errorf("a developer approving their own next link answered %d, want 403", status)
	}

	// The proof that matters is not the status code: it is that the link
	// still does not open.
	if _, err := store.RedeemDevAccess(ctx, nextToken, netip.Addr{}); err == nil {
		t.Fatal("a developer approved their own next session")
	}
}

// TestOnlyAnOwnerDecides. An admin runs a site day to day; letting
// somebody onto the machine underneath it is not that job.
func TestOnlyAnOwnerDecides(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "erisim-roller"

	owner := makeUser(t, store, "erisim-sahip3", false)
	admin := makeUser(t, store, "erisim-yonetici", false)
	viewer := makeUser(t, store, "erisim-izleyici", false)
	for _, m := range []struct {
		id   int64
		role panel.Role
	}{{owner.ID, panel.RoleOwner}, {admin.ID, panel.RoleAdmin}, {viewer.ID, panel.RoleViewer}} {
		if err := store.AddMember(ctx, site, m.id, m.role, nil); err != nil {
			t.Fatalf("AddMember(%s): %v", m.role, err)
		}
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	token, req := requestAccess(t, store, "rol-testi")

	for _, tc := range []struct {
		who   string
		email string
	}{{"an admin", admin.Email}, {"a viewer", viewer.Email}} {
		t.Run(tc.who, func(t *testing.T) {
			c := signedIn(t, server.URL, tc.email)
			if status, _ := get(t, c, server.URL+DevAccessRequestsPath); status != http.StatusForbidden {
				t.Errorf("the page answered %d to %s, want 403", status, tc.who)
			}
			status, _ := postWithToken(t, c, server.URL+DevAccessRequestsPath,
				sessionToken(t, c, server.URL), url.Values{
					"islem": {"onayla"}, "istek": {strconv.FormatInt(req.ID, 10)},
				})
			if status != http.StatusForbidden {
				t.Errorf("%s approving answered %d, want 403", tc.who, status)
			}
		})
	}

	if _, err := store.RedeemDevAccess(ctx, token, netip.Addr{}); err == nil {
		t.Fatal("the link opened after being approved by somebody who may not approve")
	}
}

// TestTwoOwnersDecidingAtOnceProduceOneDecision.
//
// Both are looking at the same banner; both click. The UPDATE's own
// WHERE clause is what settles it, and the loser has to be told the
// truth - "already decided" - rather than shown a fault.
func TestTwoOwnersDecidingAtOnceProduceOneDecision(t *testing.T) {
	_, store := setupTestServer(t)
	ctx := context.Background()
	const site = "erisim-yaris"

	owner := makeUser(t, store, "erisim-yaris-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	_, req := requestAccess(t, store, "yaris")

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refused  int
		other    []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Half approve, half deny. Any two of them succeeding would
			// mean a request that was both approved and denied.
			var err error
			if i%2 == 0 {
				err = store.ApproveDevAccess(ctx, req.ID, owner)
			} else {
				err = store.DenyDevAccess(ctx, req.ID, owner)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, panel.ErrDevAccessDecided):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range other {
		t.Errorf("unexpected failure: %v", err)
	}
	if accepted != 1 {
		t.Errorf("%d of %d decisions were accepted; exactly one may be", accepted, racers)
	}
	if refused != racers-1 {
		t.Errorf("%d refusals, want %d", refused, racers-1)
	}

	// One decision means one audit entry. A log that records a decision
	// the database refused is a log that cannot be trusted about the
	// ones it did accept.
	entries, _, err := store.Audit(ctx, panel.AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	target := "dev_access:" + strconv.FormatInt(req.ID, 10)
	decisions := 0
	for _, e := range entries {
		if e.Target != target {
			continue
		}
		if e.Action == panel.ActionDevAccessApproved || e.Action == panel.ActionDevAccessDenied {
			decisions++
		}
	}
	if decisions != 1 {
		t.Errorf("%d decisions in the audit log, want 1", decisions)
	}
}

// TestABannerReachesTheOwnerOnEveryPage.
//
// The banner lives in the shared chrome rather than on the landing page,
// because an owner working somewhere else for ten minutes is exactly who
// it is for. Checked on two unrelated pages, and checked to be absent
// for somebody who is not the one to decide - a banner drawn for
// everybody is a banner nobody reads.
func TestABannerReachesTheOwnerOnEveryPage(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "erisim-afis"

	owner := makeUser(t, store, "erisim-afis-sahip", false)
	admin := makeUser(t, store, "erisim-afis-yonetici", false)
	for _, m := range []struct {
		id   int64
		role panel.Role
	}{{owner.ID, panel.RoleOwner}, {admin.ID, panel.RoleAdmin}} {
		if err := store.AddMember(ctx, site, m.id, m.role, nil); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	banner := srv.Renderer.Catalogs().Base().T("erisim.afis.baslik")

	ownerClient := signedIn(t, server.URL, owner.Email)
	adminClient := signedIn(t, server.URL, admin.Email)

	// ---- nothing pending: no banner for anybody ----
	for _, page := range []string{"/", AccountPath} {
		_, body := get(t, ownerClient, server.URL+page)
		if strings.Contains(body, banner) {
			t.Errorf("%s shows the banner with nothing pending", page)
		}
	}

	_, req := requestAccess(t, store, "afis")

	// ---- pending: the owner sees it everywhere ----
	for _, page := range []string{"/", AccountPath, memberPath(site)} {
		_, body := get(t, ownerClient, server.URL+page)
		if !strings.Contains(body, banner) {
			t.Errorf("%s does not show the banner while a request is waiting", page)
		}
		if !strings.Contains(body, DevAccessRequestsPath) {
			t.Errorf("%s shows the banner with no way to reach the decision", page)
		}
	}

	// ---- and the admin, who cannot decide, is not nagged about it ----
	for _, page := range []string{"/", AccountPath} {
		_, body := get(t, adminClient, server.URL+page)
		if strings.Contains(body, banner) {
			t.Errorf("%s shows an admin a decision that is not theirs to make", page)
		}
	}

	// ---- decided: the banner goes away ----
	if err := store.DenyDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatalf("DenyDevAccess: %v", err)
	}
	_, body := get(t, ownerClient, server.URL+"/")
	if strings.Contains(body, banner) {
		t.Error("the banner survives the decision it was asking for")
	}
}

// TestARefusedLinkIsRecorded.
//
// A link presented after it was denied, or after it was already used, is
// the most interesting event in this whole mechanism - and until C5 it
// produced a log line and nothing an owner could ever read.
//
// The second half is the part that needed thinking about: the redemption
// URL is public, so filing an entry for every string presented would let
// a stranger write rows into an append-only table at the speed of their
// connection. A token matching nothing is somebody guessing, and is not
// recorded.
func TestARefusedLinkIsRecorded(t *testing.T) {
	_, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "erisim-ret-sahip", false)
	if err := store.AddMember(ctx, "erisim-ret", owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	token, req := requestAccess(t, store, "reddedilip-denenecek")
	if err := store.DenyDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatalf("DenyDevAccess: %v", err)
	}

	from := netip.MustParseAddr("198.51.100.7")
	if _, err := store.RedeemDevAccess(ctx, token, from); err == nil {
		t.Fatal("a denied link opened a session")
	}

	entries, _, err := store.Audit(ctx, panel.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	target := "dev_access:" + strconv.FormatInt(req.ID, 10)
	var found *panel.AuditEntry
	for i, e := range entries {
		if e.Action == panel.ActionDevAccessRejected && e.Target == target {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("using a denied link left no record an owner could read")
	}
	if found.IP == nil || found.IP.String() != from.String() {
		t.Errorf("the record does not say where it was tried from: %v", found.IP)
	}

	// ---- a token matching nothing writes nothing ----
	after := auditCount(t, store)
	if _, err := store.RedeemDevAccess(ctx, "bu-jeton-hicbir-satira-uymuyor", from); err == nil {
		t.Fatal("a made-up token opened a session")
	}
	if n := auditCount(t, store); n != after {
		t.Errorf("a guessed token wrote %d audit rows; a stranger could fill the table", n-after)
	}
}

func auditCount(t *testing.T, store *panel.Store) int {
	t.Helper()
	_, total, err := store.Audit(context.Background(), panel.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	return total
}
