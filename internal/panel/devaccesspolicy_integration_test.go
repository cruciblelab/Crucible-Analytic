//go:build integration

// C8: the deployment's standing answer to "may the developer come in".
//
// Before this the policy was implicit and fixed - auto-approve while
// nobody owned the deployment, ask the owner afterwards - and a customer
// who wanted either of the other two answers had no way to say so.
package panel

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// policyStore is a store with an owner, so the bootstrap path is not
// what is being measured.
//
// Every test here needs an account to exist: with none, RequestDevAccess
// approves on the spot whatever the policy says, which is correct and
// is tested separately below.
func policyStore(t *testing.T) (*Store, Principal) {
	t.Helper()
	s := newTestStore(t, "c8-policy")
	u := mustUser(t, s, "c8-policy", "sahip", false)

	// The policy is deployment-wide, like the upgrade lock, so a row left
	// behind changes the answer for every test after it - including in
	// other packages. Cleared at both ends for the reason
	// upgradeStore gives: a run that died before its cleanup would
	// otherwise make the next "nothing is configured" case start from
	// something configured.
	clean := func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key LIKE 'access.developer%'`); err != nil {
			t.Logf("cleanup: clearing the policy: %v", err)
		}
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_dev_access`); err != nil {
			t.Logf("cleanup: clearing dev access rows: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	return s, Principal{Kind: PrincipalUser, UserID: u.ID, Label: u.Email}
}

// invalidAddr is "no address was recorded", which is what a redemption
// from a test has.
var invalidAddr = netip.Addr{}

func TestTheDefaultPolicyIsToAskTheOwner(t *testing.T) {
	ctx := context.Background()
	s, _ := policyStore(t)

	p := s.DevAccessPolicyFor(ctx)
	if p.Mode != DevAccessAsk {
		t.Errorf("a deployment nobody has configured resolves to %q, want %q.\n"+
			"The default has to be the answer that keeps a person in the loop",
			p.Mode, DevAccessAsk)
	}

	_, req, err := s.RequestDevAccess(ctx, "sebep", 0, 0)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if !req.Pending() {
		t.Errorf("the request is not pending (approved=%v denied=%v); under the "+
			"default policy it must wait for the owner", req.ApprovedAt, req.DeniedAt)
	}
}

func TestDenyRefusesOnArrivalAndSaysSo(t *testing.T) {
	ctx := context.Background()
	s, owner := policyStore(t)

	if err := s.SetDevAccessPolicy(ctx, owner, DevAccessDeny, ""); err != nil {
		t.Fatalf("SetDevAccessPolicy: %v", err)
	}

	token, req, err := s.RequestDevAccess(ctx, "sebep", 0, 0)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if req.DeniedAt == nil {
		t.Fatal("a request under a deny policy was not denied; it would sit in the " +
			"owner's queue forever, which is the opposite of what they asked for")
	}
	if req.Pending() {
		t.Error("a denied request still reports itself pending")
	}
	// And the token it handed back is inert, which is the half that
	// matters: a refusal that still mints a usable link is not a refusal.
	if _, err := s.RedeemDevAccess(ctx, token, invalidAddr); err == nil {
		t.Error("the link minted under a deny policy was redeemable")
	}
}

func TestOpenAdmitsWithoutAskingButOnlyInsideItsWindow(t *testing.T) {
	ctx := context.Background()
	s, owner := policyStore(t)

	until := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := s.SetDevAccessPolicy(ctx, owner, DevAccessOpen, until); err != nil {
		t.Fatalf("SetDevAccessPolicy: %v", err)
	}

	token, req, err := s.RequestDevAccess(ctx, "sebep", 0, 0)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if req.ApprovedAt == nil {
		t.Fatal("a request under an open policy was not approved")
	}
	// The one that would have shipped broken. auto_approved means
	// "granted because nobody owned this yet", and RedeemDevAccess kills
	// such a row once an account exists - so an open-policy approval that
	// reused the flag would mint a link that cannot be redeemed, in
	// exactly the case where the owner said yes on purpose.
	if req.AutoApproved {
		t.Error("an open-policy approval was marked auto_approved, which is the " +
			"bootstrap flag; RedeemDevAccess refuses those once an owner exists")
	}
	grant, err := s.RedeemDevAccess(ctx, token, invalidAddr)
	if err != nil {
		t.Fatalf("the link the open policy approved could not be redeemed: %v", err)
	}
	if grant.Bootstrap {
		t.Error("the grant claims to be a bootstrap session; nobody would be able " +
			"to tell it apart from a deployment with no owner")
	}
}

func TestAnExpiredWindowFallsBackToAskingRatherThanToRefusing(t *testing.T) {
	ctx := context.Background()
	s, owner := policyStore(t)

	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	if err := s.SetDevAccessPolicy(ctx, owner, DevAccessOpen, past); err != nil {
		t.Fatalf("SetDevAccessPolicy: %v", err)
	}

	p := s.DevAccessPolicyFor(ctx)
	if p.Mode != DevAccessAsk {
		t.Errorf("an open policy whose window closed resolves to %q, want %q", p.Mode, DevAccessAsk)
	}
	if !p.Expired() {
		t.Error("the resolved policy does not report itself expired, so the settings " +
			"page cannot tell the owner why their choice stopped applying")
	}
	if p.Stored != DevAccessOpen {
		t.Errorf("the stored value reads %q; the page has to be able to show what "+
			"was actually chosen", p.Stored)
	}

	// Falling back to ask rather than to deny is the deliberate half. An
	// expired window means nobody decided anything - refusing would strand
	// the owner's own developer at the moment they are most likely to need
	// them, and the owner never asked for that.
	_, req, err := s.RequestDevAccess(ctx, "sebep", 0, 0)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if !req.Pending() {
		t.Errorf("an expired window produced approved=%v denied=%v; it must fall "+
			"back to asking", req.ApprovedAt, req.DeniedAt)
	}
}

func TestChangingThePolicyReachesTheAuditLog(t *testing.T) {
	ctx := context.Background()
	s, owner := policyStore(t)

	until := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := s.SetDevAccessPolicy(ctx, owner, DevAccessOpen, until); err != nil {
		t.Fatalf("SetDevAccessPolicy: %v", err)
	}

	entries, _, err := s.Audit(ctx, AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var found *AuditEntry
	for i := range entries {
		if entries[i].Action == ActionDevAccessPolicySet {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("changing the developer access policy wrote no audit entry.\n" +
			"C8 requires the decision to be recorded: \"why was I refused in March\" " +
			"is a question the individual refusals cannot answer")
	}
	if found.ActorLabel != owner.Label {
		t.Errorf("the entry names %q, want the owner %q", found.ActorLabel, owner.Label)
	}
	if got, _ := found.Detail["policy"].(string); got != DevAccessOpen {
		t.Errorf("the entry records policy %q, want %q", got, DevAccessOpen)
	}
	if got, _ := found.Detail["open_until"].(string); got != until {
		t.Errorf("the entry records open_until %q, want %q", got, until)
	}
}

func TestLeavingOpenClearsTheWindow(t *testing.T) {
	ctx := context.Background()
	s, owner := policyStore(t)

	until := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := s.SetDevAccessPolicy(ctx, owner, DevAccessOpen, until); err != nil {
		t.Fatalf("SetDevAccessPolicy: %v", err)
	}
	// The timestamp is sent again alongside the new mode, which is what a
	// form does: the dropdown moves to "ask" and the box still holds what
	// the owner typed last week. Passing "" here instead would have the
	// caller do the clearing and leave this test measuring nothing - the
	// first version did, and a mutation that deleted the clearing
	// altogether kept it green.
	if err := s.SetDevAccessPolicy(ctx, owner, DevAccessAsk, until); err != nil {
		t.Fatalf("SetDevAccessPolicy: %v", err)
	}

	got, err := s.GetSetting(ctx, KeyDevAccessOpenUntil, "")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if text, _ := got.(string); text != "" {
		t.Errorf("the window survived a move back to ask (%q).\n"+
			"On the settings page a leftover timestamp reads as though the "+
			"deployment is one word away from being open until then", text)
	}
}
