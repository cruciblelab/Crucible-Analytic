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
	configFile := Definition{Key: "test.configfile", Developer: true, ConfigFileOnly: true}

	cases := []struct {
		who  string
		a    Access
		def  Definition
		want SettingAccess
	}{
		{"operator, ordinary", operator, ordinary, SettingWritable},
		{"operator, developer-mode", operator, developerOwned, SettingWritable},
		{"operator, guarded", operator, guarded, SettingGated},

		{"developer session, guarded", developer, guarded, SettingGated},
		{"developer session, developer-mode", developer, developerOwned, SettingWritable},

		// The customer, with the most authority the panel can give them.
		{"owner, ordinary", owner, ordinary, SettingWritable},
		// Developer mode is a page, not a permission. A customer who
		// turns it on may change what is behind it.
		{"owner, developer-mode", owner, developerOwned, SettingWritable},
		{"owner, guarded", owner, guarded, SettingLocked},

		{"admin, developer-mode", admin, developerOwned, SettingWritable},
		{"admin, guarded", admin, guarded, SettingLocked},

		{"viewer, ordinary", viewer, ordinary, SettingReadOnly},
		{"viewer, guarded", viewer, guarded, SettingLocked},

		// A config-file setting is nobody's to change from the panel -
		// not the customer's, and not the operator's either. The panel
		// cannot honour a control over a file it does not own.
		{"operator, config file", operator, configFile, SettingReadOnly},
		{"owner, config file", owner, configFile, SettingReadOnly},
		{"viewer, config file", viewer, configFile, SettingReadOnly},
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
		"config file": LockNoticeConfigFile,
		"legal":       LockNoticeLegal,
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

	// The config-file notice explains where the value actually lives,
	// and says plainly that this is not a permission - the operator
	// cannot change it here either. A message implying "you are not
	// allowed" would send the customer asking for a right that does not
	// exist for anyone.
	for _, want := range []string{"yapılandırma dosyasında", "geliştirici de dâhil"} {
		if !strings.Contains(LockNoticeConfigFile, want) {
			t.Errorf("the config-file notice does not mention %q:\n%s", want, LockNoticeConfigFile)
		}
	}

	// A viewer's lock is not about us at all, and must not send them to
	// us for something their own owner can grant.
	if strings.Contains(LockNoticeViewer, "geliştirici") {
		t.Errorf("a viewer is told to go to the developer for a permission their owner grants:\n%s", LockNoticeViewer)
	}
}

// The registry, audited against the rule rather than against itself:
// only settings with legal or ethical weight may withhold a control from
// a customer. Anything else in developer mode is theirs to change.
//
// Written as an explicit list because the check that matters is human -
// somebody has to have decided that each of these seven really does
// decide what personal data is kept. A test that derived the list from
// the flag would only be asserting that the code equals itself.
func TestRegistry_OnlyLegallyWeightedSettingsAreWithheld(t *testing.T) {
	weighted := map[Key]bool{
		KeyPrivacyIPStorage:          true, // whether whole addresses are stored
		KeyAnalyticsRetentionDays:    true, // how long visit records are kept
		KeyLogRetentionDays:          true, // access logs contain addresses
		KeyLogImportantRetentionDays: true, // the "who got in" trail
		KeyCampaignDropParams:        true, // utm_term can carry real search text
		KeyCampaignExtraParams:       true, // stores fields we do not control
		KeyCampaignStoreClickID:      true, // a per-click permanent identifier
	}

	customer := Access{Principal: Principal{Kind: PrincipalUser}, Role: RoleOwner, Member: true}
	for _, def := range AllDefinitions() {
		editable := customer.AccessTo(def).Editable()
		switch {
		case weighted[def.Key] && editable:
			t.Errorf("%s carries legal weight but the customer may change it", def.Key)
		case !weighted[def.Key] && !editable && !def.ConfigFileOnly:
			t.Errorf("%s is withheld from the customer without carrying legal weight; "+
				"developer mode is a page, not a permission", def.Key)
		}
	}

	for key := range weighted {
		def, ok := Lookup(key)
		if !ok {
			t.Errorf("%s is listed as legally weighted but is not in the registry", key)
			continue
		}
		if !def.RequiresDeveloperPassword {
			t.Errorf("%s carries legal weight but is not password-guarded", key)
		}
	}
}

// A guarded setting belongs in developer mode too. Not for permission -
// the password does that - but because a setting deciding what personal
// data is kept has no business on a shop owner's front page next to the
// visitor count.
func TestRegistry_GuardedSettingsLiveInDeveloperMode(t *testing.T) {
	for _, key := range GuardedKeys() {
		def, _ := Lookup(key)
		if !def.Developer {
			t.Errorf("%s is guarded but not in developer mode; it would sit on the ordinary "+
				"settings page with a lock and no context", key)
		}
	}
}

// --- config-file settings, shown but never editable ---

// A secret must never carry a value, whatever the caller supplies. The
// check lives on the type rather than at the call sites, because there
// will be more call sites than people who remember the rule - so this
// test hands it exactly what a careless caller would.
func TestConfigFileSettings_NeverRenderASecret(t *testing.T) {
	careless := map[string]string{
		"collector.storage.timescale_dsn": "postgres://collector:hunter2@db/analytics",
		"beacon.timescale_dsn":            "postgres://beacon:hunter2@db/analytics",
		"panel.developer.password_hash":   "$argon2id$v=19$m=19456,t=2,p=1$abc$def",
		"beacon.listen_addr":              "127.0.0.1:8081",
	}

	var sawSecret, sawOrdinary bool
	for _, setting := range ConfigFileSettings(careless) {
		if setting.Secret {
			sawSecret = true
			if setting.Value != "" || setting.Known {
				t.Errorf("%s.%s carried a value: %q", setting.Service, setting.Key, setting.Value)
			}
			continue
		}
		if setting.Service == "beacon" && setting.Key == "listen_addr" {
			sawOrdinary = true
			if setting.Value != "127.0.0.1:8081" || !setting.Known {
				t.Errorf("an ordinary value did not come through: %q (known %v)", setting.Value, setting.Known)
			}
		}
	}
	if !sawSecret {
		t.Error("the registry lists no secrets, so this test proves nothing")
	}
	if !sawOrdinary {
		t.Error("beacon.listen_addr is missing from the registry")
	}
}

// "Not visible from here" and "empty in the file" are different facts -
// one about the deployment, one about the panel - and rendering them the
// same way would have the customer chasing a value that is set.
func TestConfigFileSettings_DistinguishUnknownFromEmpty(t *testing.T) {
	settings := ConfigFileSettings(map[string]string{"collector.mode": ""})

	var checkedEmpty, checkedUnknown bool
	for _, setting := range settings {
		switch {
		case setting.Service == "collector" && setting.Key == "mode":
			checkedEmpty = true
			if !setting.Known {
				t.Error("a supplied empty value was reported as unknown")
			}
		case setting.Service == "beacon" && setting.Key == "path_prefix":
			checkedUnknown = true
			if setting.Known {
				t.Error("a value nobody supplied was reported as known")
			}
		}
	}
	if !checkedEmpty || !checkedUnknown {
		t.Error("the registry no longer contains the keys this test relies on")
	}
}

// Every entry has to explain itself, or the list is a wall of TOML paths
// that tells a customer nothing they could not have guessed.
func TestConfigFileSettings_EveryEntryExplainsItself(t *testing.T) {
	for _, setting := range ConfigFileSettings(nil) {
		if setting.Label == "" || setting.Help == "" {
			t.Errorf("%s.%s has no label or help", setting.Service, setting.Key)
		}
	}
	if !strings.Contains(ConfigFileNotice, "değiştirilemezler") ||
		!strings.Contains(ConfigFileNotice, "geliştirici tarafından da") {
		// The notice must say this is not a permission. A customer told
		// "you may not" goes looking for who may; told "nobody does this
		// here", they know the shape of the answer.
		t.Errorf("the notice does not say the limit applies to everyone:\n%s", ConfigFileNotice)
	}
}
