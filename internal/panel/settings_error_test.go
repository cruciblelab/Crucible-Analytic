package panel

import (
	"errors"
	"strings"
	"testing"
)

// TestOnlyValidationErrorsAreSafeToShow is the test for CWE-209 in this
// package.
//
// The panel used to render err.Error() from a settings write straight
// into the page. For a validation failure that is exactly right - "this
// must be between 1 and 3650" is the whole point of validating - but the
// same call also returns wrapped database errors, and a pgx error
// carries constraint names, SQL state and sometimes the query text. The
// customer's browser was one bad write away from being shown the schema.
//
// The fix is a sentinel rather than a convention, because a convention
// here means every future caller correctly guessing which errors were
// written for a person.
func TestOnlyValidationErrorsAreSafeToShow(t *testing.T) {
	// Every way a caller can hand Validate something wrong.
	refusals := []struct {
		name  string
		key   Key
		value any
	}{
		{"int below the floor", KeyLogRetentionDays, 0},
		{"int above the ceiling", KeyLogRetentionDays, 1 << 30},
		{"int that is not a number", KeyLogRetentionDays, "otuz"},
		{"bool that is not a bool", KeyCampaignStoreClickID, "belki"},
		{"enum outside the set", KeyPrivacyIPStorage, "yarim"},
		{"string that is not a string", KeySiteName, 42},
		{"string over the length cap", KeySiteName, strings.Repeat("a", 2000)},
		{"timezone this machine cannot load", KeyPanelTimezone, "Avrupa/İstanbul"},
		{"list that is not a list", KeyBeaconSites, 7},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Validate(tc.key, tc.value)
			if err == nil {
				t.Fatal("the value was accepted")
			}
			if !errors.Is(err, ErrInvalidSetting) {
				t.Errorf("err is not marked safe to show: %v\n"+
					"a validation message the panel cannot recognise gets summarised "+
					"away, and the person who typed the value learns nothing", err)
			}
			// The message has to name the setting, or showing it at all
			// is pointless.
			if !strings.Contains(err.Error(), string(tc.key)) {
				t.Errorf("the message does not name the setting: %v", err)
			}
		})
	}
}

// TestAnUnknownKeyIsNotShownAsAValidationError. It is a programming
// mistake, not something the reader typed, and dressing it as advice
// would send them looking for a field that does not exist.
func TestAnUnknownKeyIsNotShownAsAValidationError(t *testing.T) {
	_, err := Validate(Key("boyle.bir.ayar.yok"), 1)
	if !errors.Is(err, ErrUnknownSetting) {
		t.Fatalf("err = %v, want ErrUnknownSetting", err)
	}
	if errors.Is(err, ErrInvalidSetting) {
		t.Error("an unknown key is marked as safe-to-show advice for the reader")
	}
}

// TestDatabaseFailuresAreNotMarkedSafeToShow closes the loop from the
// other side: the wrapper setSetting puts around a pgx error must not
// accidentally satisfy the sentinel.
func TestDatabaseFailuresAreNotMarkedSafeToShow(t *testing.T) {
	// The shape setSetting produces, with a driver error standing in for
	// the real one. Constructed rather than provoked so this stays a
	// unit test; the wrapping is what is being checked.
	dbErr := errors.New(`ERROR: duplicate key value violates unique constraint ` +
		`"panel_settings_pkey" (SQLSTATE 23505)`)
	wrapped := wrapStoreError(KeyBeaconSites, dbErr)

	if errors.Is(wrapped, ErrInvalidSetting) {
		t.Fatal("a database failure is marked safe to show; constraint names and " +
			"SQL state would be rendered into the customer's page")
	}
	if !errors.Is(wrapped, dbErr) {
		t.Error("the underlying error was lost, so an operator reading the log " +
			"cannot see what actually failed")
	}
}

// TestCheckRunsForEveryKind.
//
// Definition.Check says it "runs after the Kind's own checks, on the
// canonical form". That sentence was false for four kinds out of five:
// Check was called inside the KindString branch of the switch and
// nowhere else, so a validator attached to a list, an int, a bool or an
// enum was never called at all.
//
// It was found the worst possible way - by a test that expected a
// malformed network to be refused and watched it be stored - which is
// what a dead validator always looks like: not an error, an acceptance.
//
// This walks every Kind rather than the ones a setting happens to use
// today, because the next Check somebody attaches will be attached to
// whichever kind their setting is.
func TestCheckRunsForEveryKind(t *testing.T) {
	samples := map[Kind]any{
		KindInt:        5,
		KindBool:       true,
		KindString:     "bir",
		KindEnum:       "bir",
		KindStringList: []string{"bir"},
	}
	if len(samples) != 5 {
		t.Fatalf("there are %d kinds with samples; add the new one", len(samples))
	}

	for kind, sample := range samples {
		t.Run(string(kind), func(t *testing.T) {
			called := false
			def := Definition{
				Key: Key("test.check." + string(kind)), Scope: ScopeGlobal, Kind: kind,
				Min: 0, Max: 10, Enum: []string{"bir"},
				Check: func(any) error {
					called = true
					return errors.New("refused by the check")
				},
			}
			registry[def.Key] = def
			t.Cleanup(func() { delete(registry, def.Key) })

			_, err := Validate(def.Key, sample)
			if !called {
				t.Fatalf("a Check on a %s setting is never called, so it silently does nothing", kind)
			}
			if err == nil {
				t.Fatal("the Check refused the value and Validate accepted it anyway")
			}
			if !errors.Is(err, ErrInvalidSetting) {
				t.Errorf("a Check's refusal is not marked safe to show: %v", err)
			}
		})
	}
}

// TestCheckSeesTheCanonicalForm. The other half of Check's contract: it
// runs on the stored shape, not on whatever the caller passed. A
// validator written against []string must not be handed a []any because
// the value happened to arrive from JSON.
func TestCheckSeesTheCanonicalForm(t *testing.T) {
	var seen any
	def := Definition{
		Key: Key("test.check.canonical"), Scope: ScopeGlobal, Kind: KindStringList,
		Check: func(v any) error { seen = v; return nil },
	}
	registry[def.Key] = def
	t.Cleanup(func() { delete(registry, def.Key) })

	// The shape a value read back out of JSONB arrives in.
	if _, err := Validate(def.Key, []any{"bir", "iki"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen.([]string); !ok {
		t.Fatalf("Check saw %T; a list validator has to be handed a []string", seen)
	}
}
