package panel

import (
	"context"
	"fmt"
)

// Who may change which setting, and - separately - who may see it.
//
// The two questions have different answers, and collapsing them was the
// mistake this file exists to correct. The servers belong to the
// operator, so a range of settings is not the customer's to change. That
// is not a reason to hide them: a customer whose deployment behaves in a
// way they cannot see the reason for has to open a support ticket to
// learn what their own system is doing. So every setting is visible to
// anyone who can reach the settings page, and the control is what is
// withheld.
//
// Stated as the rule it is: **the value is shown, the reason is shown,
// and for the settings the operator owns there is nothing to click.**

// SettingAccess is what one principal may do with one setting.
//
// A closed set of four rather than a writable bool, because the panel
// renders each one differently and the differences matter to whoever is
// looking:
//
//	Writable  an ordinary control
//	Gated     a control plus the developer password, asked every time
//	Locked    value and reason, a lock, and no control - the operator
//	          owns this one and it carries legal weight
//	ReadOnly  value and reason, no control - the operator owns it
type SettingAccess string

const (
	// SettingWritable means this principal may change it directly.
	SettingWritable SettingAccess = "writable"
	// SettingGated means they may change it by supplying the developer
	// password, which is asked on every change.
	SettingGated SettingAccess = "gated"
	// SettingLocked means it carries legal weight and this principal is
	// not the one who answers for it. Visible, explained, not editable.
	SettingLocked SettingAccess = "locked"
	// SettingReadOnly means the operator owns it. Visible, explained,
	// not editable.
	SettingReadOnly SettingAccess = "read_only"
)

// Editable reports whether the panel should render a control at all.
func (a SettingAccess) Editable() bool { return a == SettingWritable || a == SettingGated }

// ErrSettingNotWritable is returned when a principal tries to change a
// setting that is not theirs to change.
//
// Distinct from ErrDeveloperPasswordRequired, because the answers differ:
// one is "supply the password", the other is "this is not yours to
// change, and no password you could type would make it so".
var ErrSettingNotWritable = fmt.Errorf("panel: this setting is managed by the developer")

// operator reports whether this principal is the party that runs the
// servers, as opposed to the customer whose site is on them.
//
// One condition, not two. A redeemed developer session is already
// superadmin - see developerPrincipal - so also accepting the developer
// *kind* here would add a second, weaker route to operator status that
// production never takes. Something would eventually construct a
// developer-kind principal without superadmin, and it would be treated
// as the operator by accident. One path is checkable; two are a
// question.
//
// Nobody else is the operator, including a customer who owns every site
// in the deployment.
func (a Access) operator() bool {
	return a.Principal.Superadmin
}

// AccessTo reports what this principal may do with this setting.
func (a Access) AccessTo(def Definition) SettingAccess {
	operator := a.operator()

	// Guarded settings first, so the lock is what a customer sees on
	// exactly the settings that carry legal weight - including a viewer,
	// who would otherwise see the same plain read-only rendering as an
	// ordinary developer setting and learn less than the truth.
	if def.RequiresDeveloperPassword {
		if operator && a.Can(CapManageSettings) {
			return SettingGated
		}
		return SettingLocked
	}
	if def.Developer && !operator {
		return SettingReadOnly
	}
	if !a.Can(CapManageSettings) {
		return SettingReadOnly
	}
	return SettingWritable
}

// MayAttemptDeveloperPassword reports whether it is worth showing this
// principal a password field at all.
//
// Used before anything expensive happens. A customer who posts a guess
// is refused on who they are, without an argon2 computation and without
// touching the failure counter - otherwise five guesses from a customer
// would lock the operator out of their own deployment, which is a denial
// of service dressed as a security control.
func (a Access) MayAttemptDeveloperPassword() bool {
	return a.operator() && a.Can(CapManageSettings)
}

// Lock notices. Turkish, because they are read by the customer.
const (
	// LockNoticeOperator explains an ordinary operator-owned setting.
	LockNoticeOperator = "Bu ayarı geliştirici yönetiyor. Sunucular geliştiricinin " +
		"sorumluluğunda olduğu için panelden değiştirilemez — ama gizlenmiyor: " +
		"değerini ve ne işe yaradığını burada görebilirsiniz. Değişmesi " +
		"gerekiyorsa geliştiriciye iletin."

	// LockNoticeLegal explains a guarded one. It says more, because the
	// reason is different in kind: not "we own the machine" but "this
	// decides what personal data is kept".
	LockNoticeLegal = "Bu ayar hukuki sorumluluk doğuran bir ayardır: hangi kişisel " +
		"verinin saklandığına veya ne kadar süre tutulduğuna karar verir. " +
		"Geliştirici bu alanlar için ayrı bir şifre kuralı getirdi ve şifre " +
		"sunucudaki yapılandırma dosyasında durur; panelden — tam yetkili bir " +
		"hesapla bile — değiştirilemez. Değeri ve gerekçesi burada açıkça yazılıdır."

	// LockNoticeViewer explains the ordinary "your role does not include
	// changing settings" case, which is not about the operator at all.
	LockNoticeViewer = "Bu ayarı görebilirsiniz; değiştirmek için ayar yönetimi yetkisi gerekir."
)

// SettingView is one row of the settings page: what the setting is, what
// it currently is, and what this principal may do about it.
type SettingView struct {
	Definition Definition
	// Value is what is in force - the stored value, or the default when
	// nothing is stored.
	Value any
	// Access is what this principal may do.
	Access SettingAccess
	// Lock is why there is no control, empty when there is one. Never
	// left blank for a locked row: "you cannot change this" without a
	// reason is what makes a panel feel broken rather than governed.
	Lock string
	// Reason is the legal explanation, for guarded settings only.
	Reason string
	// Source is "default", "global" or "site" - where the value in force
	// came from. A customer looking at a locked row should be able to
	// tell "nobody has ever set this" from "somebody chose this".
	Source string
}

// SettingsView builds the whole settings page for one principal and one
// site.
//
// Every setting appears, in every case. What varies is whether there is
// a control and what the explanation says.
func (s *Store) SettingsView(ctx context.Context, a Access, site string) ([]SettingView, error) {
	stored, err := s.ListSettings(ctx, site)
	if err != nil {
		return nil, err
	}
	// Keyed by setting, preferring the site row over the deployment-wide
	// one, exactly as GetSetting resolves it. If the two disagreed here
	// the page would show one value while the service used another.
	bySource := map[Key]Setting{}
	for _, set := range stored {
		if existing, ok := bySource[set.Key]; ok && existing.SiteID != "" && set.SiteID == "" {
			continue
		}
		bySource[set.Key] = set
	}

	out := make([]SettingView, 0, len(registry))
	for _, def := range AllDefinitions() {
		view := SettingView{
			Definition: def,
			Value:      def.Default,
			Access:     a.AccessTo(def),
			Source:     "default",
			Reason:     def.GateReason,
		}
		if set, ok := bySource[def.Key]; ok {
			// Re-validated on the way out, like every other read: a row
			// written by an older build must not be able to show a value
			// the current bounds would refuse.
			if value, err := Validate(def.Key, set.Value); err == nil {
				view.Value = value
				view.Source = "global"
				if set.SiteID != "" {
					view.Source = "site"
				}
			}
		}

		switch view.Access {
		case SettingLocked:
			view.Lock = LockNoticeLegal
		case SettingReadOnly:
			if def.Developer && !a.operator() {
				view.Lock = LockNoticeOperator
			} else {
				view.Lock = LockNoticeViewer
			}
		}
		out = append(out, view)
	}
	return out, nil
}
