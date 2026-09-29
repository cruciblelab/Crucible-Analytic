//go:build integration

// C7.2's second net (B3h), against a real database and a real gate.

package panel

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// operator is the authority a developer session carries.
func operator() Principal { return developerPrincipal() }

func issuedEntries(t *testing.T, s *Store, email string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM panel_audit_log WHERE action = $1 AND target = $2`,
		ActionRecoveryCodesIssued, NormalizeEmail(email)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheOperatorIssuesAMembersRecoveryCode: the whole of what an issue
// does - one code that works once, the old set dead, and a record.
func TestTheOperatorIssuesAMembersRecoveryCode(t *testing.T) {
	ns := "panel-kurtarma-ver"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	member := mustUser(t, s, ns, "uye", false)
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}
	old, err := s.GenerateRecoveryCodes(ctx, member.ID, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := testGate(t, s)

	issued, err := s.IssueRecoveryCode(ctx, site, operator(), authorizeAction(t, gate, RecoveryIssueGateAction), member.ID)
	if err != nil {
		t.Fatalf("the operator could not issue a code: %v", err)
	}
	if issued.Email != member.Email || issued.Code == "" {
		t.Fatalf("issued = %+v, want a code for %s", issued, member.Email)
	}
	// One: the old set gone, and nothing beside the code the operator saw.
	if n, err := s.CountRecoveryCodes(ctx, member.ID); err != nil || n != 1 {
		t.Errorf("%d unused codes after the issue (%v), want 1", n, err)
	}

	hash, err := HashPassword(goodPassword + "-yeni")
	if err != nil {
		t.Fatal(err)
	}
	peer := netip.MustParseAddr("203.0.113.30")
	if _, err := s.UseRecoveryCode(ctx, member.Email, old[0], hash, false, peer); !errors.Is(err, ErrRecoveryInvalid) {
		t.Errorf("an old code still works after the issue: %v", err)
	}
	result, err := s.UseRecoveryCode(ctx, member.Email, issued.Code, hash, false, peer)
	if err != nil {
		t.Fatalf("the issued code does not work: %v", err)
	}
	// And then nothing is left that the operator ever saw.
	if result.Remaining != 0 {
		t.Errorf("%d codes left after the one-time code, want 0", result.Remaining)
	}
	if _, err := s.UseRecoveryCode(ctx, member.Email, issued.Code, hash, false, peer); !errors.Is(err, ErrRecoveryInvalid) {
		t.Errorf("the one-time code worked twice: %v", err)
	}

	entries, _, err := s.Audit(ctx, AuditFilter{SiteID: site, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Action != ActionRecoveryCodesIssued {
			continue
		}
		found = true
		if e.Target != member.Email || e.ActorKind != PrincipalDeveloper || e.ActorLabel != DeveloperLabel ||
			e.Detail["self"] != false || e.Detail["count"] != float64(1) {
			t.Errorf("entry = %+v; it should say the operator issued the member's codes", e)
		}
	}
	if !found {
		t.Error("the issue left no audit entry")
	}
}

// TestOnlyTheOperatorWithThePasswordIssuesACode: every refusal, and after
// all of them the member's own codes still work and nothing was recorded.
func TestOnlyTheOperatorWithThePasswordIssuesACode(t *testing.T) {
	ns := "panel-kurtarma-ret"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site, elsewhere := "site-"+ns, "baska-"+ns
	owner := mustUser(t, s, ns, "sahip", false)
	member := mustUser(t, s, ns, "uye", false)
	gone := mustUser(t, s, ns, "eski", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := s.AddMember(ctx, site, gone.ID, RoleViewer, Grant{By: &owner.ID, Until: &past}); err != nil {
		t.Fatal(err)
	}
	mine, err := s.GenerateRecoveryCodes(ctx, member.ID, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := testGate(t, s)
	good := authorizeAction(t, gate, RecoveryIssueGateAction)
	// A password typed for something else authorizes nothing here.
	otherAction := authorizeAction(t, gate, UpgradeGateAction)

	cases := []struct {
		name  string
		actor Principal
		auth  devgate.Authorization
		site  string
		user  int64
		want  error
	}{
		{"the site's owner", principalOf(owner), good, site, member.ID, ErrIssueNeedsOperator},
		{"the operator without the password", operator(), devgate.Authorization{}, site, member.ID, ErrIssueNeedsPassword},
		{"the operator with another action's password", operator(), otherAction, site, member.ID, ErrIssueNeedsPassword},
		{"a member of another site", operator(), good, elsewhere, member.ID, ErrNotFound},
		{"an access that has ended", operator(), good, site, gone.ID, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.IssueRecoveryCode(ctx, tc.site, tc.actor, tc.auth, tc.user); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if n := issuedEntries(t, s, member.Email) + issuedEntries(t, s, gone.Email); n != 0 {
		t.Errorf("%d issue entries written by refused requests", n)
	}
	hash, err := HashPassword(goodPassword + "-yeni")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRecoveryCode(ctx, member.Email, mine[0], hash, false, netip.MustParseAddr("203.0.113.31")); err != nil {
		t.Errorf("a refused issue still changed the member's codes: %v", err)
	}
}

// TestAnIssueThatCannotBeRecordedDoesNotHappen: a code for another person,
// minted with no record of who minted it, cannot commit. The record is
// refused by an actor label PostgreSQL will not store; the old codes still
// work afterwards.
func TestAnIssueThatCannotBeRecordedDoesNotHappen(t *testing.T) {
	ns := "panel-kurtarma-kayit"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	member := mustUser(t, s, ns, "uye", false)
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}
	mine, err := s.GenerateRecoveryCodes(ctx, member.ID, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	actor := operator()
	actor.Label = "gelistirici\x00" + ns
	_, err = s.IssueRecoveryCode(ctx, site, actor, authorizeAction(t, testGate(t, s), RecoveryIssueGateAction), member.ID)
	if err == nil || !strings.Contains(err.Error(), "record audit entry") {
		t.Fatalf("err = %v; the issue should fail at its audit entry", err)
	}
	hash, err := HashPassword(goodPassword + "-yeni")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRecoveryCode(ctx, member.Email, mine[0], hash, false, netip.MustParseAddr("203.0.113.32")); err != nil {
		t.Errorf("the member's own codes stopped working though nothing was issued: %v", err)
	}
}
