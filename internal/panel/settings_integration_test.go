//go:build integration

package panel

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	t.Cleanup(func() {
		fresh, err := NewStore(context.Background(), testDatabaseURL)
		if err != nil {
			t.Logf("cleanup: reopening store: %v", err)
			return
		}
		defer fresh.Close()
		if _, err := fresh.Pool().Exec(context.Background(), `DELETE FROM panel_settings`); err != nil {
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
	if err := setGuarded(t, store, KeyAnalyticsRetentionDays, "", 30); err == nil {
		t.Error("a per-site setting accepted no site")
	}
}

// "Set it once for the deployment, override it for the one site that
// needs it" has to work without writing a row per site.
func TestSettings_SiteValueOverridesTheGlobalOne(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := setGuarded(t, store, KeyAnalyticsRetentionDays, "site-a", 30); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	if got, _ := store.GetIntSetting(ctx, KeyAnalyticsRetentionDays, "site-a"); got != 30 {
		t.Errorf("site-a = %d, want its own 30", got)
	}
	// A site with no row of its own falls through to the default.
	if got, _ := store.GetIntSetting(ctx, KeyAnalyticsRetentionDays, "site-b"); got != 90 {
		t.Errorf("site-b = %d, want the default 90", got)
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
		"privacy.ip_storage",
	}
	for _, key := range readByServices {
		def, ok := Lookup(key)
		if !ok {
			t.Errorf("a service reads %q but the panel does not define it, so nobody can change it", key)
			continue
		}
		if !def.Live && key != "logs.level" {
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

	base, _, err := store.Audit(ctx, AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	before := len(base)

	gate.Verify(ctx, devgate.Request{
		Actions: []string{GateAction(KeyPrivacyIPStorage)}, Password: "yanlis-sifre-1234",
		Actor: "denetim@example.com", ActorKind: string(PrincipalUser), Peer: "203.0.113.9",
	})
	authorize(t, gate, KeyPrivacyIPStorage)

	entries, _, err := store.Audit(ctx, AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries)-before < 2 {
		t.Fatalf("recorded %d new audit entries, want at least 2 (one refused, one granted)", len(entries)-before)
	}

	var granted, refused bool
	for _, e := range entries {
		switch e.Action {
		case ActionDevPasswordGranted:
			granted = true
		case ActionDevPasswordRefused:
			refused = true
		}
	}
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

	principal := Principal{Kind: PrincipalUser, Label: "degistiren@example.com"}
	if err := store.ApplySetting(ctx, principal, KeyPrivacyIPStorage, "", "full",
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

	prompt := PromptFor(true, GuardedKeys()...)
	if len(prompt.Reasons) != len(GuardedKeys()) {
		t.Errorf("the prompt covers %d of %d guarded settings", len(prompt.Reasons), len(GuardedKeys()))
	}
	if !strings.Contains(prompt.String(), "geliştirici") {
		t.Errorf("the prompt does not say whose rule this is:\n%s", prompt.String())
	}
	unavailable := PromptFor(false, GuardedKeys()...)
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
