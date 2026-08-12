package panel

import "testing"

func accessAs(role Role) Access {
	return Access{Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "a@example.com"}, Role: role, Member: role != ""}
}

// The authorization matrix asserted whole rather than sampled: a
// capability silently granted to viewers is exactly the bug that would
// otherwise be found by a customer, not by a test.
func TestAccess_CapabilityMatrix(t *testing.T) {
	all := []Capability{
		CapViewAnalytics, CapUseDeveloperMode, CapManageMembers,
		CapManageSettings, CapManageTokens, CapViewAudit, CapDeleteSite,
	}

	granted := map[Role]map[Capability]bool{
		RoleOwner: {
			CapViewAnalytics: true, CapUseDeveloperMode: true, CapManageMembers: true,
			CapManageSettings: true, CapManageTokens: true, CapViewAudit: true, CapDeleteSite: true,
		},
		RoleAdmin: {
			CapViewAnalytics: true, CapUseDeveloperMode: true, CapManageMembers: true,
			CapManageSettings: true, CapManageTokens: true, CapViewAudit: true,
			// Not CapDeleteSite: the one thing an owner keeps to itself.
		},
		RoleViewer: {
			CapViewAnalytics: true,
		},
	}

	for role, want := range granted {
		for _, c := range all {
			if got := accessAs(role).Can(c); got != want[c] {
				t.Errorf("%s.Can(%s) = %v, want %v", role, c, got, want[c])
			}
		}
	}
}

// No membership row means no authority whatsoever. Without this, an
// unknown or removed member would fall through to whatever the zero
// Role happened to permit.
func TestAccess_NoMembershipGrantsNothing(t *testing.T) {
	none := accessAs("")
	for _, c := range []Capability{CapViewAnalytics, CapUseDeveloperMode, CapManageMembers, CapDeleteSite} {
		if none.Can(c) {
			t.Errorf("a principal with no membership was granted %s", c)
		}
	}
}

// A role this build does not recognize - a downgrade, or a hand-edited
// row - must deny rather than resolve to some default.
func TestAccess_UnknownRoleGrantsNothing(t *testing.T) {
	if accessAs(Role("superuser")).Can(CapViewAnalytics) {
		t.Error("an unrecognized role was granted analytics access")
	}
}

func TestAccess_SuperadminReachesEverySite(t *testing.T) {
	// No membership row at all, which is the normal state for the
	// operator.
	staff := Access{Principal: Principal{Kind: PrincipalUser, UserID: 1, Superadmin: true}}
	for _, c := range []Capability{CapViewAnalytics, CapManageMembers, CapDeleteSite, CapManageTokens} {
		if !staff.Can(c) {
			t.Errorf("a superadmin was denied %s", c)
		}
	}
}

// Two conditions, not one: the role says whether this person may ever
// see fingerprints and addresses, the preference says whether they want
// to right now.
func TestAccess_ShowsTechnicalNeedsBothRoleAndPreference(t *testing.T) {
	cases := []struct {
		name    string
		role    Role
		devMode bool
		want    bool
	}{
		{"owner with it on", RoleOwner, true, true},
		{"owner with it off", RoleOwner, false, false},
		{"admin with it on", RoleAdmin, true, true},
		{"viewer with it on", RoleViewer, true, false},
		{"viewer with it off", RoleViewer, false, false},
		{"no membership with it on", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := accessAs(tc.role)
			a.Principal.DeveloperMode = tc.devMode
			if got := a.ShowsTechnical(); got != tc.want {
				t.Errorf("ShowsTechnical() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A viewer turning the preference on must still see nothing technical.
// The preference is stored per user, so a viewer who was previously an
// admin can carry it - and it must not become a lingering grant.
func TestAccess_ViewerCannotUnlockTechnicalViewsWithThePreference(t *testing.T) {
	viewer := accessAs(RoleViewer)
	viewer.Principal.DeveloperMode = true
	if viewer.ShowsTechnical() {
		t.Error("a viewer with developer_mode still set from a previous role saw the technical views")
	}
	if viewer.Can(CapUseDeveloperMode) {
		t.Error("a viewer was granted CapUseDeveloperMode")
	}
}

// Nobody may grant authority above their own, or the owner/admin
// distinction is decorative: an admin could promote themselves, or
// promote an ally and be promoted back.
func TestAccess_CanAssignNeverEscalates(t *testing.T) {
	owner := accessAs(RoleOwner)
	admin := accessAs(RoleAdmin)
	viewer := accessAs(RoleViewer)

	if !owner.CanAssign(RoleOwner) {
		t.Error("an owner may not grant ownership")
	}
	if admin.CanAssign(RoleOwner) {
		t.Error("an admin was allowed to grant ownership")
	}
	if !admin.CanAssign(RoleAdmin) || !admin.CanAssign(RoleViewer) {
		t.Error("an admin may not grant admin or viewer")
	}
	for _, role := range ValidRoles {
		if viewer.CanAssign(role) {
			t.Errorf("a viewer was allowed to grant %s", role)
		}
	}

	staff := Access{Principal: Principal{Superadmin: true}}
	if !staff.CanAssign(RoleOwner) {
		t.Error("a superadmin may not grant ownership")
	}
}

func TestAccess_CanAssignRejectsUnknownRoles(t *testing.T) {
	if accessAs(RoleOwner).CanAssign(Role("god")) {
		t.Error("an unrecognized role was assignable")
	}
	if (Access{Principal: Principal{Superadmin: true}}).CanAssign(Role("god")) {
		t.Error("a superadmin was allowed to assign an unrecognized role")
	}
}

func TestRole_Valid(t *testing.T) {
	for _, role := range ValidRoles {
		if !role.Valid() {
			t.Errorf("%s is in ValidRoles but reports invalid", role)
		}
	}
	for _, role := range []Role{"", "OWNER", "superadmin", "root"} {
		if role.Valid() {
			t.Errorf("%q was accepted as a role", role)
		}
	}
}

// A one-time developer session carries operator authority and says so.
// If this ever silently became a normal user, the audit log would stop
// distinguishing staff access from a customer's own.
func TestDeveloperPrincipal_IsLabelledAndPrivileged(t *testing.T) {
	p := developerPrincipal()
	if p.Kind != PrincipalDeveloper {
		t.Errorf("Kind = %q, want %q", p.Kind, PrincipalDeveloper)
	}
	if p.Label != DeveloperLabel {
		t.Errorf("Label = %q, want %q", p.Label, DeveloperLabel)
	}
	if p.UserID != 0 {
		t.Error("a developer session claimed a user id; there is no account behind it")
	}
	if !p.Superadmin || !p.DeveloperMode {
		t.Error("a developer session must arrive with operator authority and technical views already on")
	}
}
