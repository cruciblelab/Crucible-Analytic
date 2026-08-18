package web

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// allDevAccessStates is every state a row can be drawn in.
//
// Listed here and checked against the rows the function actually
// produces, rather than trusted: a state with no catalog entry renders
// the key itself as a badge, and the ones a reader is least likely to
// see are the ones nobody notices are broken.
var allDevAccessStates = []devAccessState{
	stateWaiting, stateApproved, stateDenied, stateUsed, stateExpired, stateBootstrap,
}

// TestEveryStateHasWordsInEveryLanguage.
//
// The label is looked up as "erisim.durum." + the state, assembled at
// runtime, so neither the template walk nor the source scan in the ui
// package can see the family. That package carries a mirrored list for
// its dead-key check; this is the other direction, and it reads the real
// constants rather than a copy of them.
func TestEveryStateHasWordsInEveryLanguage(t *testing.T) {
	srv := newTestServer(t)
	for _, lang := range srv.Renderer.Catalogs().Languages() {
		for _, state := range allDevAccessStates {
			key := "erisim.durum." + string(state)
			if !lang.Has(key) {
				t.Errorf("%s has no label for the %q state (%s)", lang.Code, state, key)
			}
		}
	}
}

// TestARowIsDrawnAsWhatActuallyHappened walks every combination the
// database can hold and checks the state it lands in.
//
// The order of the tests inside devAccessRowFor is the whole content of
// that function, and every one of the pairs below is a way to get it
// wrong: a used link that has since expired is still "used", a denial is
// not undone by time passing, and an auto-approved row is never drawn as
// approved on a page that only exists once somebody has an account.
func TestARowIsDrawnAsWhatActuallyHappened(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	later := now.Add(time.Hour)
	addr := netip.MustParseAddr("203.0.113.9")

	cases := []struct {
		name string
		req  panel.DevAccessRequest
		want devAccessState
		// decidable is the property the buttons are drawn from. Exactly
		// one state has it, and getting that wrong either offers a
		// decision the database will refuse or hides one it would accept.
		decidable bool
	}{
		{
			name:      "waiting, inside its window",
			req:       panel.DevAccessRequest{RequestExpiresAt: later},
			want:      stateWaiting,
			decidable: true,
		},
		{
			name: "approved and not yet used",
			req:  panel.DevAccessRequest{RequestExpiresAt: later, ApprovedAt: &earlier},
			want: stateApproved,
		},
		{
			name: "denied",
			req:  panel.DevAccessRequest{RequestExpiresAt: later, DeniedAt: &earlier},
			want: stateDenied,
		},
		{
			name: "used",
			req: panel.DevAccessRequest{
				RequestExpiresAt: later, ApprovedAt: &earlier, UsedAt: &earlier, UsedFrom: &addr,
			},
			want: stateUsed,
		},
		{
			name: "used, and the window has since closed",
			req: panel.DevAccessRequest{
				RequestExpiresAt: earlier, ApprovedAt: &earlier, UsedAt: &earlier,
			},
			want: stateUsed,
		},
		{
			name: "denied, and the window has since closed",
			req:  panel.DevAccessRequest{RequestExpiresAt: earlier, DeniedAt: &earlier},
			want: stateDenied,
		},
		{
			name: "nobody decided in time",
			req:  panel.DevAccessRequest{RequestExpiresAt: earlier},
			want: stateExpired,
		},
		{
			// The one that would be easiest to get wrong, and the most
			// dangerous: this row carries approved_at, so a naive check
			// draws it as approved and waiting to be used. It is dead -
			// this page cannot be reached without an account existing,
			// and an account existing is what kills it.
			name: "auto-approved during install, never used",
			req: panel.DevAccessRequest{
				RequestExpiresAt: later, ApprovedAt: &earlier, AutoApproved: true,
			},
			want: stateBootstrap,
		},
		{
			name: "auto-approved during install, used",
			req: panel.DevAccessRequest{
				RequestExpiresAt: later, ApprovedAt: &earlier, AutoApproved: true, UsedAt: &earlier,
			},
			want: stateBootstrap,
		},
	}

	seen := map[devAccessState]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := devAccessRowFor(lang, tc.req, now)
			if row.State != tc.want {
				t.Errorf("state = %q, want %q", row.State, tc.want)
			}
			if row.Decidable != tc.decidable {
				t.Errorf("Decidable = %v, want %v", row.Decidable, tc.decidable)
			}
			if row.StateLabel == "" || strings.Contains(row.StateLabel, "erisim.durum") {
				t.Errorf("the badge would render as a raw key: %q", row.StateLabel)
			}
			seen[row.State] = true
		})
	}

	for _, state := range allDevAccessStates {
		if !seen[state] {
			t.Errorf("no case produces the %q state, so nothing checks how it is drawn", state)
		}
	}
}

// TestABootstrapGrantNamesNobody.
//
// ApprovedLabel is empty on a bootstrap row today, because nothing sets
// it - but the column exists and a future write could fill it, and a
// page saying "approved by X" about a grant nobody consented to is a
// lie with a name attached to it. Cleared explicitly rather than assumed
// empty.
func TestABootstrapGrantNamesNobody(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	now := time.Now()
	at := now.Add(-time.Hour)

	row := devAccessRowFor(lang, panel.DevAccessRequest{
		RequestExpiresAt: now.Add(time.Hour),
		ApprovedAt:       &at,
		AutoApproved:     true,
		ApprovedLabel:    "birisi@example.invalid",
	}, now)

	if row.DecidedBy != "" {
		t.Errorf("a bootstrap grant is credited to %q; nobody approved it", row.DecidedBy)
	}
}

// TestTheApprovalPageIsNotOfferedToADeveloper.
//
// The navigation is cosmetic - requireDecider is the lock - but a
// developer session carries Superadmin, so a nav that asked about
// ownership alone would draw them the link. This asserts the cheap half;
// the integration suite asserts the half that matters.
func TestTheApprovalPageIsNotOfferedToADeveloper(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()

	// decides=false is what decidesDevAccess returns for a developer,
	// whatever they own - see the Kind check there.
	items := srv.navFor(lang, panel.Access{Principal: panel.Principal{
		Kind:       panel.PrincipalDeveloper,
		Superadmin: true,
	}}, "siteler", false)

	for _, item := range items {
		if item.URL == DevAccessRequestsPath {
			t.Fatal("the developer is offered the page that decides developer access")
		}
	}
}

// TestAnOwnerIsOfferedThePage is the other half. A link nobody is shown
// is a page nobody finds, and the history of who asked for access is
// exactly what an owner should be able to go and read.
func TestAnOwnerIsOfferedThePage(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()

	items := srv.navFor(lang, panel.Access{Principal: panel.Principal{
		Kind: panel.PrincipalUser, UserID: 1, Label: "sahip@example.invalid",
	}}, "siteler", true)

	for _, item := range items {
		if item.URL == DevAccessRequestsPath {
			return
		}
	}
	t.Fatal("an owner is not offered the approval page anywhere in the navigation")
}
