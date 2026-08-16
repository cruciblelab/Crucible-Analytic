package panel

import (
	"strings"
	"testing"
)

// The rule, as a table: who sees what, and who may touch it.
//
// The customer is the interesting column. They hold every right the panel
// grants on their own site, the servers are still not theirs, and both
// halves of that have to be true at once - they see the value, and there
// is nothing to click.
func TestAccessTo(t *testing.T) {
	operator := Access{Principal: Principal{Kind: PrincipalUser, Label: "operator", Superadmin: true}}
	// Shaped like the real thing: a redeemed developer session is
	// superadmin (see developerPrincipal), which is what makes it the
	// operator. Constructing one without that would be testing a
	// principal production never produces.
	developer := Access{Principal: Principal{
		Kind: PrincipalDeveloper, Label: DeveloperLabel, Superadmin: true,
	}}
	owner := Access{Principal: Principal{Kind: PrincipalUser, Label: "owner"}, Role: RoleOwner, Member: true}
	admin := Access{Principal: Principal{Kind: PrincipalUser, Label: "admin"}, Role: RoleAdmin, Member: true}
	viewer := Access{Principal: Principal{Kind: PrincipalUser, Label: "viewer"}, Role: RoleViewer, Member: true}

	ordinary := Definition{Key: "test.ordinary"}
	developerOwned := Definition{Key: "test.developer", Developer: true}
	guarded := Definition{Key: "test.guarded", Developer: true, RequiresDeveloperPassword: true}

	cases := []struct {
		who  string
		a    Access
		def  Definition
		want SettingAccess
	}{
		{"operator, ordinary", operator, ordinary, SettingWritable},
		{"operator, developer-owned", operator, developerOwned, SettingWritable},
		{"operator, guarded", operator, guarded, SettingGated},

		{"developer session, guarded", developer, guarded, SettingGated},
		{"developer session, developer-owned", developer, developerOwned, SettingWritable},

		// The customer, with the most authority the panel can give them.
		{"owner, ordinary", owner, ordinary, SettingWritable},
		{"owner, developer-owned", owner, developerOwned, SettingReadOnly},
		{"owner, guarded", owner, guarded, SettingLocked},

		{"admin, developer-owned", admin, developerOwned, SettingReadOnly},
		{"admin, guarded", admin, guarded, SettingLocked},

		{"viewer, ordinary", viewer, ordinary, SettingReadOnly},
		{"viewer, guarded", viewer, guarded, SettingLocked},
	}

	for _, tc := range cases {
		t.Run(tc.who, func(t *testing.T) {
			if got := tc.a.AccessTo(tc.def); got != tc.want {
				t.Errorf("AccessTo = %q, want %q", got, tc.want)
			}
		})
	}
}

// Nothing is ever hidden. Every principal who reaches the settings page
// sees every setting; what varies is whether there is a control.
func TestAccessTo_NoSettingIsInvisibleToAnyone(t *testing.T) {
	for _, a := range []Access{
		{Principal: Principal{Superadmin: true}},
		{Principal: Principal{}, Role: RoleOwner},
		{Principal: Principal{}, Role: RoleViewer},
		{Principal: Principal{}}, // no role at all
	} {
		for _, def := range AllDefinitions() {
			access := a.AccessTo(def)
			switch access {
			case SettingWritable, SettingGated, SettingLocked, SettingReadOnly:
			default:
				t.Errorf("%s produced %q, which is not one of the four renderable states", def.Key, access)
			}
		}
	}
}

// Only the operator may even try the password. A customer's attempt has
// to cost nothing, because the failure counter is shared: five guesses
// from a customer would otherwise lock the operator out of a deployment
// the operator is responsible for.
func TestMayAttemptDeveloperPassword(t *testing.T) {
	cases := map[string]struct {
		a    Access
		want bool
	}{
		"operator":          {Access{Principal: Principal{Superadmin: true}}, true},
		"developer session": {Access{Principal: Principal{Kind: PrincipalDeveloper, Superadmin: true}}, true},
		// A developer-kind principal that is somehow not superadmin is
		// not the operator. There is one route to operator status and
		// this is not it.
		"developer kind without superadmin": {Access{Principal: Principal{Kind: PrincipalDeveloper}}, false},
		"site owner":        {Access{Principal: Principal{}, Role: RoleOwner}, false},
		"site admin":        {Access{Principal: Principal{}, Role: RoleAdmin}, false},
		"viewer":            {Access{Principal: Principal{}, Role: RoleViewer}, false},
		"nobody":            {Access{}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.a.MayAttemptDeveloperPassword(); got != tc.want {
				t.Errorf("MayAttemptDeveloperPassword = %v, want %v", got, tc.want)
			}
		})
	}
}

// A customer must never be shown a password field - it invites them to
// go looking for a password they cannot have, and every attempt they
// make spends part of a budget that is not theirs.
func TestPromptFor_ShowsTheCustomerALockRatherThanAField(t *testing.T) {
	owner := Access{Principal: Principal{Kind: PrincipalUser, Label: "owner"}, Role: RoleOwner}

	prompt := PromptFor(owner, true, KeyPrivacyIPStorage)
	if prompt.Entitled {
		t.Error("a site owner was marked entitled to attempt the developer password")
	}
	if prompt.Locked == "" {
		t.Fatal("no lock explanation was produced for the customer")
	}
	if len(prompt.Reasons) == 0 {
		// The lock says "you cannot change this"; the reason says what
		// the setting decides. Without the second, the panel reads as
		// arbitrary rather than governed.
		t.Error("the customer is shown a lock with no reason")
	}

	// And with the deployment's password missing, the customer is still
	// told about the lock rather than about a configuration gap that is
	// not theirs to close.
	unconfigured := PromptFor(owner, false, KeyPrivacyIPStorage)
	if got := unconfigured.String(); got != unconfigured.Locked+reasonsSuffix(unconfigured) {
		t.Errorf("the customer was shown the operator's configuration message:\n%s", got)
	}

	operator := Access{Principal: Principal{Superadmin: true}}
	if !PromptFor(operator, true, KeyPrivacyIPStorage).Entitled {
		t.Error("the operator was not marked entitled")
	}
}

// reasonsSuffix rebuilds what String appends, so the assertion above
// tests which opening sentence was chosen rather than re-testing the
// formatting.
func reasonsSuffix(p DeveloperPasswordPrompt) string {
	out := ""
	for _, reason := range p.Reasons {
		out += "\n\n- " + reason.Label + ": " + reason.Reason
	}
	return out
}

// A customer stopped at a control has three questions, and the notice
// has to answer all three or it produces a support ticket instead of an
// understanding: what is this, why can't I, and what do I do now.
//
// The third is the one usually left out, and it is the one that decides
// whether the customer feels governed or stonewalled.
func TestLockNotices_SayWhyAndWhatToDoNext(t *testing.T) {
	for name, notice := range map[string]string{
		"operator-owned": LockNoticeOperator,
		"legal":          LockNoticeLegal,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(notice, "bize iletin") {
				t.Errorf("the notice does not tell the customer what to do instead:\n%s", notice)
			}
			if !strings.Contains(notice, "biz yaparız") {
				// "We will connect and do it" - the reassurance that the
				// thing they want is possible, just not by them.
				t.Errorf("the notice does not say we will do it for them:\n%s", notice)
			}
		})
	}

	// The operator-owned notice explains the actual risk rather than
	// asserting ownership. "The servers are ours" is true and useless;
	// "changing this disturbs how the system runs" is the reason.
	for _, want := range []string{"sunucudaki", "işleyişini bozar", "teknik"} {
		if !strings.Contains(LockNoticeOperator, want) {
			t.Errorf("the operator notice does not mention %q:\n%s", want, LockNoticeOperator)
		}
	}

	// A viewer's lock is not about us at all, and must not send them to
	// us for something their own owner can grant.
	if strings.Contains(LockNoticeViewer, "geliştirici") {
		t.Errorf("a viewer is told to go to the developer for a permission their owner grants:\n%s", LockNoticeViewer)
	}
}

// Every setting in the registry is currently operator-owned, so a
// customer's settings page is entirely read-only. That is the intended
// state today, not an oversight - but it makes the writable path
// unreachable for a customer, so it is asserted rather than assumed. A
// future customer-facing setting (a timezone, a dashboard preference)
// will make this fail, and the failure is the reminder to check that it
// really is theirs to change.
func TestRegistry_EverySettingIsCurrentlyOperatorOwned(t *testing.T) {
	customer := Access{Principal: Principal{Kind: PrincipalUser}, Role: RoleOwner, Member: true}
	for _, def := range AllDefinitions() {
		if access := customer.AccessTo(def); access.Editable() {
			t.Errorf("%s is customer-writable (%s); if that is intended, this test needs updating "+
				"and the setting needs a reason it is safe for a customer to change", def.Key, access)
		}
	}
}

// A guarded setting must also be operator-owned. The password is the
// stronger guard, but if it were ever lifted the setting should fall
// back to read-only rather than all the way to customer-writable.
func TestRegistry_GuardedSettingsDegradeToOperatorOwned(t *testing.T) {
	for _, key := range GuardedKeys() {
		def, _ := Lookup(key)
		if !def.Developer {
			t.Errorf("%s is guarded but not marked Developer; removing the password guard "+
				"would hand it straight to the customer", key)
		}
	}
}
