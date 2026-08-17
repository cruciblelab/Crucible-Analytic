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

// ErrPreconditionUnmet is returned when a value is refused not because
// of who asked, but because the deployment is not in a state where it
// could be honoured.
//
// Separate from the other two errors because the answer differs again:
// not "supply the password" and not "this is not yours", but "somebody
// has to put something on the server first".
var ErrPreconditionUnmet = fmt.Errorf("panel: the deployment is not configured for this value")

// SetIPTokenKeyConfigured records whether the deployment has an IP token
// key in its config file.
//
// The panel cannot read the collector's and beacon's config files, so
// the binary that starts the panel tells it once at wiring time. Default
// false, which is the safe direction: without the key, switching to full
// mode would write masked rows and no tokens, and the deployment would
// silently be in masked mode while its setting said otherwise.
func (s *Store) SetIPTokenKeyConfigured(configured bool) { s.ipTokenKeyConfigured = configured }

// IPTokenKeyConfigured reports what it was told.
func (s *Store) IPTokenKeyConfigured() bool { return s.ipTokenKeyConfigured }

// checkPrecondition refuses a value the deployment could not honour.
//
// Only one setting has a precondition today, and it earns it: full mode
// without a key does not fail, it *degrades* - and a mode that silently
// becomes a different mode is the worst way for this particular setting
// to be wrong.
func (s *Store) checkPrecondition(key Key, value any) error {
	if key != KeyPrivacyIPStorage {
		return nil
	}
	if mode, ok := value.(string); ok && mode == IPStorageFull && !s.ipTokenKeyConfigured {
		return fmt.Errorf("%w: %s requires privacy.ip_hash_key in the config file first "+
			"(generate one with: go run ./cmd/devpass -ipkey)", ErrPreconditionUnmet, IPStorageFull)
	}
	return nil
}

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
//
// Three questions in order, and only the first two can withhold a
// control:
//
//  1. Does it live in a config file? Then nobody edits it here, the
//     operator included - the panel cannot honour a control it does not
//     own.
//  2. Does it carry legal or ethical weight? Then only the operator may
//     change it, and only against the password, every time.
//  3. Otherwise it is an ordinary setting, and whoever may manage
//     settings may change it - customer included, developer mode or
//     not.
//
// Being a developer-mode setting is not one of the questions. It decides
// which page a setting appears on, not who may touch it.
func (a Access) AccessTo(def Definition) SettingAccess {
	if def.ConfigFileOnly {
		return SettingReadOnly
	}
	if def.RequiresDeveloperPassword {
		if a.operator() && a.Can(CapManageSettings) {
			return SettingGated
		}
		return SettingLocked
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

// Lock notices. Turkish, because the customer reads them.
//
// Each says three things, in this order: that the setting is not theirs
// to change, *why* - in terms of what would actually go wrong - and what
// to do instead. The last part matters as much as the first. A customer
// told only "you cannot change this" is a customer who concludes the
// panel is broken or that we are being difficult; a customer told "tell
// us and we will connect and do it" has been given the actual route.
const (
	// LockNoticeConfigFile explains a setting that lives in a config file.
	//
	// Not a permission at all, which is why it says so plainly. Nobody
	// changes this from the panel - the operator sees the same message.
	// A deployment's listen address or database credentials are read
	// once at startup from a file on disk, and a panel offering to edit
	// them would be offering something it cannot deliver.
	LockNoticeConfigFile = "Bu ayar sunucudaki yapılandırma dosyasında durur, veritabanında " +
		"değil — bu yüzden panelden kimse değiştiremez, geliştirici de dâhil. " +
		"Değeri burada görünür, çünkü kurulumunuzun nasıl çalıştığını görebilmeniz " +
		"gerekir. Değişmesi gerekiyorsa bize iletin: sunucuya bağlanıp biz yaparız."

	// LockNoticeLegal explains a guarded one. It says more, because the
	// reason is different in kind: not "this could break something" but
	// "this decides what personal data is kept".
	LockNoticeLegal = "Bu ayar hukuki sorumluluk doğuran bir ayardır: hangi kişisel " +
		"verinin saklandığına veya ne kadar süre tutulduğuna karar verir. " +
		"Geliştirici bu alanlar için ayrı bir şifre kuralı getirdi ve şifre " +
		"sunucudaki yapılandırma dosyasında durur; panelden — tam yetkili bir " +
		"hesapla bile — değiştirilemez. Değeri ve gerekçesi burada açıkça yazılıdır. " +
		"Değişmesi gerekiyorsa bize iletin: sunucuya bağlanıp biz yaparız."

	// LockNoticeViewer explains the ordinary "your role does not include
	// changing settings" case, which is not about the operator at all -
	// and must not be worded as if it were, or a viewer would go asking
	// us for something their own owner can grant.
	LockNoticeViewer = "Bu ayarı görebilirsiniz; değiştirmek için ayar yönetimi yetkisi gerekir. " +
		"Bu yetkiyi kendi hesabınızın sahibi verebilir."
)

// DeveloperModeNotice is what the dashboard shows when somebody opens
// developer mode.
//
// A warning rather than a barrier, and the difference is the whole
// point. What is behind the toggle is bot scoring, device
// fingerprinting, rate windows and DDoS detection - readings that mean
// something to whoever built them and mean something *else* to whoever
// reads them cold. A shop owner wants how many people came and from
// where; handed a JA4 hash and a score of 61, they will reach a
// conclusion, and it will be the wrong one.
//
// So the panel says so, once, at the door - and then lets them in. They
// own the deployment. Locking the page would be deciding on their behalf
// what they are allowed to understand about their own system, which is
// not ours to decide.
const DeveloperModeNotice = "Geliştirici ayarları teknik ölçümler içerir: bot olasılık " +
	"skoru, cihaz/TLS parmak izi, istek hızı pencereleri, saldırı tespiti. " +
	"Bu değerler nasıl hesaplandıkları bilinmeden yorumlandığında yanıltıcı " +
	"olabilir — bir sayının yüksek olması tek başına bir sorun anlamına " +
	"gelmez. Teknik bilginiz yoksa bu bölümdeki ayarları değiştirmemenizi " +
	"öneririz; görüntülemek serbesttir ve hiçbir riski yoktur. Emin " +
	"olmadığınız bir ayar için bize sorun."

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
			if def.ConfigFileOnly {
				view.Lock = LockNoticeConfigFile
			} else {
				view.Lock = LockNoticeViewer
			}
		}
		out = append(out, view)
	}
	return out, nil
}
