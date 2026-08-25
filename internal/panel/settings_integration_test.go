//go:build integration

package panel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// settingsStore opens a store and clears panel_settings afterwards.
// Settings are global by nature, so unlike the user tests there is no
// namespace to scope cleanup to - the table is emptied instead, which is
// safe because nothing else in the suite writes to it.
func settingsStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t, "settings")
	// A deployment that has an IP token key on disk, which is what
	// switching to full mode requires. The tests that care about the
	// refusal turn it off explicitly.
	store.SetIPTokenKeyConfigured(true)
	t.Cleanup(func() {
		fresh, err := NewStore(context.Background(), testDatabaseURL)
		if err != nil {
			t.Logf("cleanup: reopening store: %v", err)
			return
		}
		defer fresh.Close()
		// Everything except the "test." namespace, which internal/settings'
		// live suite owns. Both suites share this table and `go test ./...`
		// runs them in parallel, so a bare DELETE here would clear rows
		// that suite is in the middle of reading.
		if _, err := fresh.Pool().Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key NOT LIKE 'test.%'`); err != nil {
			t.Logf("cleanup: clearing panel_settings: %v", err)
		}
	})
	return store
}

func TestSettings_DefaultsApplyBeforeAnythingIsStored(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	// A deployment that has never touched settings still gets working
	// numbers, rather than zeroes that would mean "delete everything".
	days, err := store.GetIntSetting(ctx, KeyLogRetentionDays, "")
	if err != nil {
		t.Fatalf("GetIntSetting: %v", err)
	}
	if days != 14 {
		t.Errorf("logs.retention_days = %d, want the default 14", days)
	}
	important, err := store.GetIntSetting(ctx, KeyLogImportantRetentionDays, "")
	if err != nil {
		t.Fatalf("GetIntSetting: %v", err)
	}
	if important != 365 {
		t.Errorf("logs.important_retention_days = %d, want the default 365", important)
	}
}

func TestSettings_StoreAndReadBack(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := setGuarded(t, store, KeyLogRetentionDays, "", 30); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	got, err := store.GetIntSetting(ctx, KeyLogRetentionDays, "")
	if err != nil {
		t.Fatalf("GetIntSetting: %v", err)
	}
	if got != 30 {
		t.Errorf("read back %d, want 30", got)
	}

	// Resetting brings the default back rather than storing a zero.
	if err := resetGuarded(t, store, KeyLogRetentionDays, ""); err != nil {
		t.Fatalf("ResetSetting: %v", err)
	}
	if got, _ := store.GetIntSetting(ctx, KeyLogRetentionDays, ""); got != 14 {
		t.Errorf("after reset = %d, want the default 14", got)
	}
}

// The bounds are enforced when writing, not when reading: a stored value
// no reader will accept is a trap for whoever restarts the service next.
func TestSettings_RefusesValuesOutsideTheirBounds(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	for _, bad := range []any{0, -1, 4000, "thirty", 1.5} {
		if err := setGuarded(t, store, KeyLogRetentionDays, "", bad); err == nil {
			t.Errorf("SetSetting accepted %v (%T) for a 1..3650 setting", bad, bad)
		}
	}
	// And nothing was written.
	if got, _ := store.GetIntSetting(ctx, KeyLogRetentionDays, ""); got != 14 {
		t.Errorf("a refused write still changed the value to %d", got)
	}
}

func TestSettings_RefusesAnUnknownKey(t *testing.T) {
	store := settingsStore(t)
	if err := store.SetSetting(context.Background(), Key("logs.made_up"), "", 5, nil); err == nil {
		t.Error("SetSetting accepted a key nobody defined")
	}
}

func TestSettings_RefusesAnEnumValueOutsideItsSet(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := store.SetSetting(ctx, KeyLogLevel, "", "verbose", nil); err == nil {
		t.Error("SetSetting accepted a level outside the enum")
	}
	if err := store.SetSetting(ctx, KeyLogLevel, "", "debug", nil); err != nil {
		t.Errorf("SetSetting refused a valid level: %v", err)
	}
}

// Scope mismatches are refused rather than quietly stored under the
// wrong one, where the value would read back as "unset" forever.
func TestSettings_RefusesAScopeMismatch(t *testing.T) {
	store := settingsStore(t)

	if err := setGuarded(t, store, KeyLogRetentionDays, "somesite", 30); err == nil {
		t.Error("a global setting accepted a site")
	}
	if err := setGuarded(t, store, KeySiteName, "", "Bir Site"); err == nil {
		t.Error("a per-site setting accepted no site")
	}
}

// "Give one site its own value, leave the rest on the default" has to
// work without writing a row per site.
//
// Written against analytics.retention_days until that key moved to the
// config files, and now against the site name - which is the last
// site-scoped key with a scalar value, so this is also the only
// remaining test of the fall-through itself.
func TestSettings_SiteValueOverridesTheGlobalOne(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := setGuarded(t, store, KeySiteName, "site-a", "Bir Site"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	if got, _ := store.GetSetting(ctx, KeySiteName, "site-a"); got != "Bir Site" {
		t.Errorf("site-a = %v, want its own %q", got, "Bir Site")
	}
	// A site with no row of its own falls through to the default rather
	// than reading the other site's.
	if got, _ := store.GetSetting(ctx, KeySiteName, "site-b"); got != "" {
		t.Errorf("site-b = %v, want the default empty name", got)
	}
}

// A row written by an older build, or edited by hand, must not hand a
// service a value outside the bounds it was written against.
func TestSettings_OutOfBoundsStoredValueFallsBackToTheDefault(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	// Bypass validation the way a hand edit would.
	_, err := store.Pool().Exec(ctx, `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, '99999'::jsonb)`, string(KeyLogRetentionDays))
	if err != nil {
		t.Fatalf("hand-inserting: %v", err)
	}

	got, err := store.GetIntSetting(ctx, KeyLogRetentionDays, "")
	if err != nil {
		t.Fatalf("GetIntSetting: %v", err)
	}
	if got != 14 {
		t.Errorf("an out-of-bounds stored value was returned as %d; want the default 14", got)
	}
}

// Archiving after deletion would lose a day's logs before they were ever
// compressed, and that is not recoverable.
func TestLogLifecycle_ClampsArchiveBeforeRetention(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := setGuarded(t, store, KeyLogRetentionDays, "", 5); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := store.SetSetting(ctx, KeyLogArchiveAfterDays, "", 30, nil); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	archiveAfter, retention, important, err := store.LogLifecycle(ctx)
	if err != nil {
		t.Fatalf("LogLifecycle: %v", err)
	}
	if archiveAfter > retention {
		t.Errorf("archiveAfter %d > retention %d; a day would be deleted before it was compressed", archiveAfter, retention)
	}
	if important != 365 {
		t.Errorf("important retention = %d, want 365", important)
	}
}

func TestSettings_ListReportsWhatWasStored(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := setGuarded(t, store, KeyLogRetentionDays, "", 21); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	list, err := store.ListSettings(ctx, "")
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	found := false
	for _, s := range list {
		if s.Key == KeyLogRetentionDays {
			found = true
			if s.UpdatedAt.IsZero() {
				t.Error("the stored setting carries no timestamp")
			}
		}
	}
	if !found {
		t.Errorf("ListSettings did not return the stored setting: %+v", list)
	}
}

// The two lists of setting keys - the panel's registry and the names
// internal/settings reads - must agree. A key the panel defines that no
// service reads is a setting that does nothing; a key a service reads
// that the panel does not define is a setting nobody can change.
//
// Checked here rather than by importing internal/settings into the
// panel: the dependency deliberately points one way, so a beacon never
// drags the panel's data layer into the one process the whole internet
// can reach.
func TestSettings_LiveKeysMatchWhatServicesRead(t *testing.T) {
	// The names internal/settings/live.go declares, copied. If this list
	// and that one drift, one of them is wrong and this test says so.
	readByServices := []Key{
		"beacon.sites",
		"campaign.drop_params",
		"campaign.extra_params",
		"campaign.store_click_ids",
		"logs.level",
		"logs.verbose_until",
		"privacy.ip_storage",

		// A5.1.
		"beacon.trusted_proxies",
		"collector.limits.max_concurrent",
		"collector.limits.max_requests_per_second",
		"collector.limits.overload_policy",
		"collector.limits.throttle_queue_size",
		"beacon.limits.max_concurrent",
		"beacon.limits.max_requests_per_second",
		"beacon.limits.overload_policy",
		"beacon.limits.throttle_queue_size",

		// A5.2.
		"collector.blocked_countries",
		"collector.blocked_asns",
		"collector.known_bot_asns",
		"collector.apply_asn_to_scoring",
	}
	for _, key := range readByServices {
		def, ok := Lookup(key)
		if !ok {
			t.Errorf("a service reads %q but the panel does not define it, so nobody can change it", key)
			continue
		}
		// No exemptions. There was one - `key != "logs.level"` - carrying
		// no reason, which meant the check skipped the single line it
		// would have caught. A test that excuses the case it exists for
		// is worse than no test: it reports a clean run.
		if !def.Live {
			t.Errorf("%q is read live by a service but is not marked Live, so the panel will "+
				"tell a customer it needs a restart when it does not", key)
		}
	}

	// And the reverse: everything marked Live must actually be read.
	read := map[Key]bool{}
	for _, key := range readByServices {
		read[key] = true
	}
	for _, def := range AllDefinitions() {
		if def.Live && !read[def.Key] {
			t.Errorf("%q is marked Live but no service reads it, so the panel promises an "+
				"immediate effect that never happens", def.Key)
		}
	}
}

// --- the developer password gate (A7.5) ---

const testDevPassword = "test-gelistirici-sifresi"

// testGate builds a real gate with a real argon2id hash, so nothing here
// is testing a stand-in for the mechanism.
func testGate(t *testing.T, store *Store) *devgate.Gate {
	t.Helper()
	hash, err := argon2id.Hash(testDevPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	gate, err := devgate.New(devgate.Config{PasswordHash: hash}, devgate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:  store.GateAudit(),
	})
	if err != nil {
		t.Fatalf("devgate.New: %v", err)
	}
	return gate
}

// authorize passes the gate for one setting and returns the
// authorization, failing the test if the gate refused.
func authorize(t *testing.T, gate *devgate.Gate, key Key) devgate.Authorization {
	t.Helper()
	result := gate.Verify(context.Background(), devgate.Request{
		Actions:   []string{GateAction(key)},
		Password:  testDevPassword,
		Actor:     "test@example.com",
		ActorKind: string(PrincipalUser),
		Peer:      "203.0.113.9",
	})
	if !result.OK() {
		t.Fatalf("the gate refused a correct password: %s", result.Decision)
	}
	return result.For(GateAction(key))
}

// setGuarded writes a setting through the gate, for the tests that are
// about validation rather than about the gate.
func setGuarded(t *testing.T, store *Store, key Key, site string, value any) error {
	t.Helper()
	gate := testGate(t, store)
	return store.SetGuardedSetting(context.Background(), key, site, value, nil, authorize(t, gate, key))
}

func resetGuarded(t *testing.T, store *Store, key Key, site string) error {
	t.Helper()
	gate := testGate(t, store)
	return store.ResetGuardedSetting(context.Background(), key, site, authorize(t, gate, key))
}

// The enforcement point is the write path, not the handler. A call site
// added later cannot forget a check it is unable to compile without.
func TestGuardedSettings_CannotBeWrittenThroughTheOrdinaryPath(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	for _, key := range GuardedKeys() {
		def, _ := Lookup(key)
		site := ""
		if def.Scope == ScopeSite {
			site = "site-a"
		}

		err := store.SetSetting(ctx, key, site, defaultValueFor(t, def), nil)
		if !errors.Is(err, ErrDeveloperPasswordRequired) {
			t.Errorf("%s was writable without the developer password (err = %v)", key, err)
		}
		if err := store.ResetSetting(ctx, key, site); !errors.Is(err, ErrDeveloperPasswordRequired) {
			// Resetting is a change of value like any other, and for
			// campaign.drop_params the default is the empty list - so
			// "reset" would mean "start storing utm_term again".
			t.Errorf("%s was resettable without the developer password (err = %v)", key, err)
		}
	}
}

func TestGuardedSettings_WriteWithAValidAuthorization(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)

	auth := authorize(t, gate, KeyPrivacyIPStorage)
	if err := store.SetGuardedSetting(ctx, KeyPrivacyIPStorage, "", "full", nil, auth); err != nil {
		t.Fatalf("SetGuardedSetting: %v", err)
	}
	got, err := store.GetStringSetting(ctx, KeyPrivacyIPStorage, "")
	if err != nil {
		t.Fatalf("GetStringSetting: %v", err)
	}
	if got != "full" {
		t.Errorf("read back %q, want full", got)
	}
}

// The default is masked, and that is the answer the lawyer gave. A
// deployment that never touches this setting stores masked addresses.
func TestPrivacyIPStorage_DefaultsToMasked(t *testing.T) {
	store := settingsStore(t)

	got, err := store.GetStringSetting(context.Background(), KeyPrivacyIPStorage, "")
	if err != nil {
		t.Fatalf("GetStringSetting: %v", err)
	}
	if got != IPStorageMasked {
		t.Errorf("privacy.ip_storage = %q, want %q", got, IPStorageMasked)
	}
}

// An authorization earned for one setting must not be spendable on
// another, all the way through to the database.
func TestGuardedSettings_AnAuthorizationForOneSettingDoesNotWriteAnother(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)

	auth := authorize(t, gate, KeyLogRetentionDays)
	err := store.SetGuardedSetting(ctx, KeyPrivacyIPStorage, "", "full", nil, auth)
	if !errors.Is(err, ErrDeveloperPasswordRequired) {
		t.Fatalf("an authorization for logs.retention_days wrote privacy.ip_storage (err = %v)", err)
	}
	if got, _ := store.GetStringSetting(ctx, KeyPrivacyIPStorage, ""); got != IPStorageMasked {
		t.Errorf("the refused write still changed the value to %q", got)
	}
}

func TestGuardedSettings_AWrongPasswordWritesNothing(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)

	result := gate.Verify(ctx, devgate.Request{
		Actions:   []string{GateAction(KeyPrivacyIPStorage)},
		Password:  "yanlis-sifre-1234",
		Actor:     "test@example.com",
		ActorKind: string(PrincipalUser),
		Peer:      "203.0.113.9",
	})
	if result.OK() {
		t.Fatal("the gate accepted a wrong password")
	}

	err := store.SetGuardedSetting(ctx, KeyPrivacyIPStorage, "", "full", nil, result.For(GateAction(KeyPrivacyIPStorage)))
	if !errors.Is(err, ErrDeveloperPasswordRequired) {
		t.Fatalf("a refused verification still wrote (err = %v)", err)
	}
	if got, _ := store.GetStringSetting(ctx, KeyPrivacyIPStorage, ""); got != IPStorageMasked {
		t.Errorf("the value changed to %q despite a wrong password", got)
	}
}

// Every attempt reaches the append-only audit log, successful or not.
func TestGuardedSettings_EveryAttemptIsAudited(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)

	// Counted by action against the database directly, not by the length
	// of a capped page. The page-length version passed until the audit
	// table grew past the limit, at which point both reads returned the
	// same 200 rows and the difference was always zero - a test that
	// stops testing once the system has been used a while is worse than
	// no test, because it keeps reporting success.
	countAttempts := func() (granted, refused int) {
		t.Helper()
		if err := store.Pool().QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE action = $1), count(*) FILTER (WHERE action = $2)
			FROM panel_audit_log WHERE actor_label = $3`,
			ActionDevPasswordGranted, ActionDevPasswordRefused, "denetim@example.com").
			Scan(&granted, &refused); err != nil {
			t.Fatalf("counting audit entries: %v", err)
		}
		return granted, refused
	}
	grantedBefore, refusedBefore := countAttempts()

	gate.Verify(ctx, devgate.Request{
		Actions: []string{GateAction(KeyPrivacyIPStorage)}, Password: "yanlis-sifre-1234",
		Actor: "denetim@example.com", ActorKind: string(PrincipalUser), Peer: "203.0.113.9",
	})
	gate.Verify(ctx, devgate.Request{
		Actions: []string{GateAction(KeyPrivacyIPStorage)}, Password: testDevPassword,
		Actor: "denetim@example.com", ActorKind: string(PrincipalUser), Peer: "203.0.113.9",
	})

	grantedAfter, refusedAfter := countAttempts()
	granted, refused := grantedAfter > grantedBefore, refusedAfter > refusedBefore
	if !granted {
		t.Error("the successful attempt was not audited")
	}
	if !refused {
		// The failures are the interesting rows: a run of them is
		// somebody working at it, which is invisible if only successes
		// are recorded.
		t.Error("the refused attempt was not audited")
	}
}

// ApplySetting records what the value was as well as what it became.
// "Who set the retention to 3650 days" is asked months later, when the
// only thing anybody remembers is that it used to be different.
func TestApplySetting_RecordsTheOldValueAndTheNew(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)

	operator := Access{Principal: Principal{
		Kind: PrincipalUser, Label: "degistiren@example.com", Superadmin: true,
	}}
	if err := store.ApplySetting(ctx, operator, KeyPrivacyIPStorage, "", "full",
		authorize(t, gate, KeyPrivacyIPStorage)); err != nil {
		t.Fatalf("ApplySetting: %v", err)
	}

	entries, _, err := store.Audit(ctx, AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, e := range entries {
		if e.Action != ActionSettingChanged || e.Target != string(KeyPrivacyIPStorage) {
			continue
		}
		if e.Detail["from"] != IPStorageMasked {
			t.Errorf("the entry recorded from = %v, want %q", e.Detail["from"], IPStorageMasked)
		}
		if e.Detail["to"] != "full" {
			t.Errorf("the entry recorded to = %v, want full", e.Detail["to"])
		}
		if e.Detail["guarded"] != true {
			t.Error("the entry does not record that this setting was guarded")
		}
		return
	}
	t.Error("no setting.changed entry was recorded")
}

// With no developer password configured, guarded settings stay at their
// defaults - which are the privacy-preserving values, so nothing worth
// having is lost.
func TestGuardedSettings_WithNoPasswordConfiguredNothingCanBeChanged(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	gate, err := devgate.New(devgate.Config{}, devgate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:  store.GateAudit(),
	})
	if err != nil {
		t.Fatalf("devgate.New: %v", err)
	}

	result := gate.Verify(ctx, devgate.Request{
		Actions: []string{GateAction(KeyPrivacyIPStorage)}, Password: "herhangi-bir-sifre",
		Actor: "test@example.com", ActorKind: string(PrincipalUser),
	})
	if result.OK() {
		t.Fatal("an unconfigured gate granted an authorization")
	}
	if err := store.SetGuardedSetting(ctx, KeyPrivacyIPStorage, "", "full", nil,
		result.For(GateAction(KeyPrivacyIPStorage))); !errors.Is(err, ErrDeveloperPasswordRequired) {
		t.Errorf("a guarded setting was writable with no password configured (err = %v)", err)
	}
	if got, _ := store.GetStringSetting(ctx, KeyPrivacyIPStorage, ""); got != IPStorageMasked {
		t.Errorf("the value is %q, want the default %q", got, IPStorageMasked)
	}
}

// Every guarded setting has to say why it is guarded. "This needs a
// password" invites somebody to go looking for the password; "this
// decides whether whole addresses are stored" tells them what they are
// about to do.
func TestGuardedSettings_EachOneExplainsItself(t *testing.T) {
	for _, key := range GuardedKeys() {
		def, _ := Lookup(key)
		if def.GateReason == "" {
			t.Errorf("%s is guarded but gives no reason", key)
		}
	}
	if len(GuardedKeys()) == 0 {
		t.Fatal("nothing is guarded; the gate protects nothing")
	}

	operator := Access{Principal: Principal{Kind: PrincipalUser, Superadmin: true}}
	prompt := PromptFor(operator, true, GuardedKeys()...)
	if len(prompt.Reasons) != len(GuardedKeys()) {
		t.Errorf("the prompt covers %d of %d guarded settings", len(prompt.Reasons), len(GuardedKeys()))
	}
	if !strings.Contains(prompt.String(), "geliştirici") {
		t.Errorf("the prompt does not say whose rule this is:\n%s", prompt.String())
	}
	unavailable := PromptFor(operator, false, GuardedKeys()...)
	if !strings.Contains(unavailable.String(), "password_hash") {
		t.Errorf("with no password configured the prompt does not say how to configure one:\n%s", unavailable.String())
	}
}

// defaultValueFor returns something valid to attempt writing, so the
// "was it refused" assertions cannot pass merely because the value was
// invalid.
func defaultValueFor(t *testing.T, def Definition) any {
	t.Helper()
	switch def.Kind {
	case KindInt:
		return def.Min
	case KindBool:
		return true
	case KindEnum:
		return def.Enum[0]
	case KindString:
		return "x"
	case KindStringList:
		return []string{"x"}
	}
	t.Fatalf("no test value for kind %q", def.Kind)
	return nil
}

// --- who may change what (A7.6) ---

func customerAccess() Access {
	// The most authority the panel can give a customer: owner of their
	// own site, with a real membership row. UserID stays zero so the
	// audit row records the label without a foreign key to a user this
	// test never created.
	return Access{
		Principal: Principal{Kind: PrincipalUser, Label: "musteri@example.com"},
		Role:      RoleOwner,
		Member:    true,
	}
}

func operatorAccess() Access {
	return Access{Principal: Principal{Kind: PrincipalUser, Label: "operator@crucible", Superadmin: true}}
}

// The customer sees every setting, including the ones they cannot touch,
// with the value that is actually in force. Hiding them would leave them
// unable to account for their own deployment.
//
// "Cannot touch" now means exactly one thing: the setting carries legal
// weight, or it lives in a config file. Being a developer-mode setting is
// not one of them.
func TestSettingsView_ShowsTheCustomerEverything(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := setGuarded(t, store, KeyPrivacyIPStorage, "", IPStorageFull); err != nil {
		t.Fatalf("setGuarded: %v", err)
	}

	view, err := store.SettingsView(ctx, customerAccess(), "site-a")
	if err != nil {
		t.Fatalf("SettingsView: %v", err)
	}
	if len(view) != len(AllDefinitions()) {
		t.Fatalf("the customer sees %d of %d settings", len(view), len(AllDefinitions()))
	}

	var seen bool
	for _, row := range view {
		if row.Definition.Key != KeyPrivacyIPStorage {
			continue
		}
		seen = true
		if row.Value != IPStorageFull {
			t.Errorf("the customer sees %v, want the value in force (%q)", row.Value, IPStorageFull)
		}
		if row.Source != "global" {
			t.Errorf("Source = %q, want global - the customer cannot tell a default from a choice", row.Source)
		}
		if row.Access != SettingLocked {
			t.Errorf("Access = %q, want %q", row.Access, SettingLocked)
		}
		if row.Lock == "" {
			t.Error("a locked row carries no explanation, which is what makes a panel feel broken")
		}
		if row.Reason == "" {
			t.Error("a locked row does not say what the setting decides")
		}
	}
	if !seen {
		t.Error("privacy.ip_storage did not appear in the customer's view at all")
	}

	// And every locked or read-only row explains itself. A control that
	// is simply absent, with no sentence, is indistinguishable from a bug.
	for _, row := range view {
		if !row.Access.Editable() && row.Lock == "" {
			t.Errorf("%s is not editable and gives no reason", row.Definition.Key)
		}
		if !row.Access.Editable() && !row.Definition.RequiresDeveloperPassword && !row.Definition.ConfigFileOnly {
			t.Errorf("%s is withheld from the customer without carrying legal weight "+
				"and without living in a config file", row.Definition.Key)
		}
		if row.Access.Editable() && row.Lock != "" {
			t.Errorf("%s is editable but carries a lock notice", row.Definition.Key)
		}
	}
}

func TestSettingsView_GivesTheOperatorControls(t *testing.T) {
	store := settingsStore(t)

	view, err := store.SettingsView(context.Background(), operatorAccess(), "site-a")
	if err != nil {
		t.Fatalf("SettingsView: %v", err)
	}
	for _, row := range view {
		want := SettingWritable
		if row.Definition.RequiresDeveloperPassword {
			want = SettingGated
		}
		if row.Access != want {
			t.Errorf("%s: operator access = %q, want %q", row.Definition.Key, row.Access, want)
		}
	}
}

// The refusal is on identity, before the password is considered at all -
// so no password the customer could supply changes the answer.
func TestApplySetting_RefusesTheCustomerWhateverTheySupply(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)
	customer := customerAccess()

	// Even handed a genuinely valid authorization, minted elsewhere.
	auth := authorize(t, gate, KeyPrivacyIPStorage)
	err := store.ApplySetting(ctx, customer, KeyPrivacyIPStorage, "", IPStorageFull, auth)
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Fatalf("the customer changed a guarded setting (err = %v)", err)
	}
	if got, _ := store.GetStringSetting(ctx, KeyPrivacyIPStorage, ""); got != IPStorageMasked {
		t.Errorf("the value became %q despite the refusal", got)
	}

	// But a developer-mode setting that carries no legal weight *is*
	// theirs. Developer mode decides which page a setting appears on,
	// not who may touch it, and asserting the opposite here was the
	// mistake this test now guards against.
	unweighted := Access{
		Principal: Principal{Kind: PrincipalUser, Label: "musteri@example.com"},
		Role:      RoleOwner, Member: true,
	}
	if err := store.ApplySetting(ctx, unweighted, KeyLogArchiveAfterDays, "", 3, devgate.Authorization{}); err != nil {
		t.Errorf("the customer could not change a setting with no legal weight: %v", err)
	}
	if got, _ := store.GetIntSetting(ctx, KeyLogArchiveAfterDays, ""); got != 3 {
		t.Errorf("logs.archive_after_days = %d, want 3", got)
	}

	// And clearing is a change like any other.
	if err := store.ClearSetting(ctx, customer, KeyPrivacyIPStorage, "", auth); !errors.Is(err, ErrSettingNotWritable) {
		t.Errorf("the customer reset a guarded setting (err = %v)", err)
	}
}

// A customer's attempt must not spend the operator's failure budget.
// Five guesses from a customer locking the operator out of a deployment
// they are responsible for would be a denial of service wearing the
// clothes of a security control.
func TestGateRequest_ACustomersGuessCostsNothing(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	gate := testGate(t, store)

	form := url.Values{devgate.FormField: {"yanlis-sifre-1234"}}
	post := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "203.0.113.9:5555"
		return r
	}

	// Far more than the lockout threshold.
	for i := 0; i < 20; i++ {
		result := gate.Verify(ctx, store.GateRequest(customerAccess(), post(), KeyPrivacyIPStorage))
		if result.OK() {
			t.Fatal("a customer's guess was granted")
		}
		if result.Decision == devgate.DecisionWrongPassword {
			t.Fatalf("attempt %d reached argon2; the customer's guess was hashed", i+1)
		}
	}

	// The operator is still able to work.
	if result := gate.Verify(ctx, devgate.Request{
		Actions: []string{GateAction(KeyPrivacyIPStorage)}, Password: testDevPassword,
	}); !result.OK() {
		t.Errorf("the customer's guesses locked the operator out: %s", result.Decision)
	}
}

// Full mode without a key does not fail, it *degrades*: the writers
// would store the masked address and no token, so the deployment would
// silently be in masked mode while its setting said otherwise. A mode
// that quietly becomes a different mode is the worst way for this
// particular setting to be wrong, so the panel refuses the value.
func TestApplySetting_FullModeNeedsTheKeyOnDiskFirst(t *testing.T) {
	store := settingsStore(t)
	store.SetIPTokenKeyConfigured(false)
	ctx := context.Background()
	gate := testGate(t, store)
	operator := operatorAccess()

	err := store.ApplySetting(ctx, operator, KeyPrivacyIPStorage, "", IPStorageFull,
		authorize(t, gate, KeyPrivacyIPStorage))
	if !errors.Is(err, ErrPreconditionUnmet) {
		t.Fatalf("full mode was accepted with no key configured (err = %v)", err)
	}
	if !strings.Contains(err.Error(), "devpass -ipkey") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
	if got, _ := store.GetStringSetting(ctx, KeyPrivacyIPStorage, ""); got != IPStorageMasked {
		t.Errorf("the refused write still changed the value to %q", got)
	}

	// The refusal is about the deployment, not the value: masked is
	// always available, because its default needs nothing on disk.
	if err := store.ApplySetting(ctx, operator, KeyPrivacyIPStorage, "", IPStorageMasked,
		authorize(t, gate, KeyPrivacyIPStorage)); err != nil {
		t.Errorf("masked mode was refused: %v", err)
	}

	// And once the key is there, the same write goes through - so this
	// is a precondition, not a prohibition.
	store.SetIPTokenKeyConfigured(true)
	if err := store.ApplySetting(ctx, operator, KeyPrivacyIPStorage, "", IPStorageFull,
		authorize(t, gate, KeyPrivacyIPStorage)); err != nil {
		t.Errorf("full mode was refused with the key configured: %v", err)
	}
}
