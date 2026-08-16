//go:build integration

package panel

import (
	"context"
	"testing"
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

	if err := store.SetSetting(ctx, KeyLogRetentionDays, "", 30, nil); err != nil {
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
	if err := store.ResetSetting(ctx, KeyLogRetentionDays, ""); err != nil {
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
		if err := store.SetSetting(ctx, KeyLogRetentionDays, "", bad, nil); err == nil {
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
	ctx := context.Background()

	if err := store.SetSetting(ctx, KeyLogRetentionDays, "somesite", 30, nil); err == nil {
		t.Error("a global setting accepted a site")
	}
	if err := store.SetSetting(ctx, KeyAnalyticsRetentionDays, "", 30, nil); err == nil {
		t.Error("a per-site setting accepted no site")
	}
}

// "Set it once for the deployment, override it for the one site that
// needs it" has to work without writing a row per site.
func TestSettings_SiteValueOverridesTheGlobalOne(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()

	if err := store.SetSetting(ctx, KeyAnalyticsRetentionDays, "site-a", 30, nil); err != nil {
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

	if err := store.SetSetting(ctx, KeyLogRetentionDays, "", 5, nil); err != nil {
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

	if err := store.SetSetting(ctx, KeyLogRetentionDays, "", 21, nil); err != nil {
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
