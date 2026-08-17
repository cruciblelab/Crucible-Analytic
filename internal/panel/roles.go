package panel

// Role is what one account may do on one site.
//
// Sites are the tenant boundary of this whole system: one server can
// host several customers, and site A's owner must never be able to see
// site B. There is deliberately no cross-site aggregate view anywhere in
// the panel - not because it would be hard, but because "these three
// customers' traffic combined" is not a number any of the three should
// be shown.
type Role string

const (
	// RoleOwner has full authority over one site, including removing
	// other members and deleting the site's panel configuration.
	RoleOwner Role = "owner"
	// RoleAdmin may manage members and settings but cannot delete the
	// site - so an owner can delegate day-to-day administration without
	// handing over the ability to destroy it.
	RoleAdmin Role = "admin"
	// RoleViewer may look at the dashboard and nothing else. In
	// particular a viewer never reaches the technical views, regardless
	// of any preference they set: those expose individual addresses and
	// fingerprints, which is a different kind of access from "how many
	// people visited".
	RoleViewer Role = "viewer"
)

// ValidRoles is every assignable role, in descending authority. Used to
// validate input and to render the role picker in a stable order.
var ValidRoles = []Role{RoleOwner, RoleAdmin, RoleViewer}

// Valid reports whether r is a role this system recognizes. Anything
// arriving from a form or a database row goes through here first: an
// unrecognized role must never fall through to a permissive default.
func (r Role) Valid() bool {
	for _, known := range ValidRoles {
		if r == known {
			return true
		}
	}
	return false
}

// Capability is one thing a principal might be allowed to do. Handlers
// ask for a capability rather than checking a role directly, so that
// changing who may do what is a change to one table below rather than a
// hunt through every handler.
type Capability string

const (
	// CapViewAnalytics covers the ordinary dashboard: visitors,
	// pageviews, sources, devices. What a shop owner logs in for.
	CapViewAnalytics Capability = "view_analytics"
	// CapUseDeveloperMode covers everything behind the developer toggle:
	// TLS fingerprints, ASNs, bot scores, per-address detail, the
	// cross-source views and raw export. Gated by role *and* by the
	// user's own preference - see Access.ShowsTechnical.
	CapUseDeveloperMode Capability = "use_developer_mode"
	// CapManageMembers covers adding, removing and re-roling members.
	CapManageMembers Capability = "manage_members"
	// CapManageSettings covers a site's panel settings.
	CapManageSettings Capability = "manage_settings"
	// CapManageTokens covers minting and revoking API tokens. Separate
	// from CapManageSettings because a token is a credential that
	// outlives the session it was created in.
	CapManageTokens Capability = "manage_tokens"
	// CapViewAudit covers reading the audit log for a site.
	CapViewAudit Capability = "view_audit"
	// CapDeleteSite covers removing a site's panel configuration. Owner
	// only - it is the one action an admin should not be able to take.
	CapDeleteSite Capability = "delete_site"
)

// roleCapabilities is the whole authorization model, in one place on
// purpose. Anything not listed for a role is denied: the lookup below
// treats a missing entry as false rather than falling back to a default,
// so adding a capability without deciding who gets it fails closed.
var roleCapabilities = map[Role]map[Capability]bool{
	RoleOwner: {
		CapViewAnalytics:    true,
		CapUseDeveloperMode: true,
		CapManageMembers:    true,
		CapManageSettings:   true,
		CapManageTokens:     true,
		CapViewAudit:        true,
		CapDeleteSite:       true,
	},
	RoleAdmin: {
		CapViewAnalytics:    true,
		CapUseDeveloperMode: true,
		CapManageMembers:    true,
		CapManageSettings:   true,
		CapManageTokens:     true,
		CapViewAudit:        true,
	},
	RoleViewer: {
		CapViewAnalytics: true,
	},
}

// PrincipalKind distinguishes who is acting.
type PrincipalKind string

const (
	// PrincipalUser is an ordinary account.
	PrincipalUser PrincipalKind = "user"
	// PrincipalDeveloper is a one-time developer login, redeemed from a
	// link minted on the server itself. It has no account behind it,
	// which is why it needs its own kind rather than being represented
	// as some shared user row: the audit log must be able to say "this
	// was a developer session", not "this was somebody logged in as
	// admin@example.com".
	PrincipalDeveloper PrincipalKind = "developer"
	// PrincipalSystem is the panel acting on its own behalf - startup,
	// cleanup, first-run setup. Never authenticates a request; exists so
	// audit entries with no human behind them are honest about it.
	PrincipalSystem PrincipalKind = "system"
)

// Principal is who is making a request, resolved once by the
// authentication middleware and carried on the request context.
type Principal struct {
	Kind PrincipalKind
	// UserID is 0 for developer and system principals.
	UserID int64
	// Label is what the audit log records: an email for a user, a fixed
	// marker for a developer session.
	Label string
	// Superadmin means the operator hosting this deployment, as opposed
	// to a customer who owns one site. Superadmins reach every site
	// without needing a membership row.
	Superadmin bool
	// DeveloperMode is the user's own preference, off by default. It is
	// only ever honored alongside CapUseDeveloperMode - see
	// Access.ShowsTechnical.
	DeveloperMode bool
}

// DeveloperLabel is the audit-log identity of a one-time developer
// session. A fixed string rather than a name, because there is no
// account to name and inventing one would make the log look like an
// ordinary user did the work.
const DeveloperLabel = "Geliştirici"

// developerPrincipal is what a redeemed developer access link becomes.
//
// It carries superadmin authority, and that is honest rather than a
// shortcut: whoever redeemed it can run commands on the server, so they
// can already read the config, the database password, and therefore
// everything. Pretending to restrict what they see inside the panel
// would add friction and no security.
//
// The controls that do matter are elsewhere, in devaccess.go: before
// anyone owns the deployment the link is granted automatically, because
// installing the system is the job and there is nobody to ask; once an
// account exists it is inert until that owner approves it, and any
// outstanding bootstrap link dies at that same moment. Access during
// installation does not quietly become access afterwards. On top of
// that the link is single-use, time-boxed, and everything it does is
// filed under a visibly separate identity.
func developerPrincipal() Principal {
	return Principal{
		Kind:       PrincipalDeveloper,
		Label:      DeveloperLabel,
		Superadmin: true,
		// On by default: a developer session exists to look at technical
		// detail, so making them find the toggle first would be pure
		// ceremony.
		DeveloperMode: true,
	}
}

// Access is one principal's authority over one specific site, resolved
// by looking up their membership. Handlers receive this rather than a
// bare Role, so "superadmin, no membership row" and "viewer with a
// membership row" are answered by the same call.
type Access struct {
	Principal Principal
	// Role is the membership role, or "" when the principal has no
	// membership on this site (which is normal for a superadmin).
	Role Role
	// Member reports whether a membership row actually exists, as
	// distinct from access granted by being a superadmin. Used by the UI
	// to say "you are seeing this as the operator".
	Member bool
	// SiteID is the site this decision was made about.
	//
	// Carried on the decision rather than passed alongside it, because
	// an Access and a site id travelling as separate arguments can be
	// separated - and a handler that authorises against one site and
	// then reads another is the bug this field exists to make
	// unwriteable.
	SiteID string
}

// Can reports whether this principal may perform c on this site.
//
// A superadmin may do anything, on every site. That is what hosting the
// deployment means, and hiding it behind per-site membership rows would
// only mean the operator quietly granting themselves one whenever they
// needed to - the same power with worse visibility.
func (a Access) Can(c Capability) bool {
	if a.Principal.Superadmin {
		return true
	}
	return roleCapabilities[a.Role][c]
}

// ShowsTechnical reports whether the technical views should be rendered:
// the principal must be allowed to use developer mode *and* have turned
// it on.
//
// Two conditions rather than one because they answer different
// questions. The role says whether this person may ever see individual
// addresses and fingerprints; the preference says whether they want to
// right now. Collapsing them would either show a shop owner a JA4 hash
// they cannot act on, or make a developer's toggle silently do nothing.
func (a Access) ShowsTechnical() bool {
	return a.Can(CapUseDeveloperMode) && a.Principal.DeveloperMode
}

// CanAssign reports whether this principal may grant the given role.
//
// Nobody may grant authority above their own. Without this an admin
// could make themselves an owner, or make someone else one and be
// promoted back - which would make the owner/admin distinction
// decorative.
func (a Access) CanAssign(role Role) bool {
	if !a.Can(CapManageMembers) || !role.Valid() {
		return false
	}
	if a.Principal.Superadmin {
		return true
	}
	if role == RoleOwner {
		return a.Role == RoleOwner
	}
	return true
}

// RoleCan reports whether a bare role carries a capability.
//
// Access.Can is the one to use in a handler: it knows about superadmins,
// and it was resolved for a specific site. This exists for the few
// questions that are about a role in the abstract - "does any site this
// person is a member of let them use developer mode" - where there is no
// single site to resolve against.
//
// Exported so that answering such a question does not require rebuilding
// an Access with a fabricated principal, which is the shape mistakes
// take: a fabricated principal is one field away from being a
// fabricated superadmin.
func RoleCan(r Role, c Capability) bool { return roleCapabilities[r][c] }
