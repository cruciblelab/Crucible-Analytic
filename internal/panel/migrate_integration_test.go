//go:build integration

package panel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeConfig puts a TOML file on disk for the migration to read.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clearMigrated removes the rows a migration test writes, before and
// after.
//
// Before as well as after, because a run that was interrupted - a
// database that went away mid-suite, a test binary killed - leaves rows
// behind, and the next run then reads them as though this test had
// written them. Cleaning only on the way out makes a suite that passes
// or fails depending on how the previous one ended.
func clearMigrated(t *testing.T, s *Store, keys ...Key) {
	t.Helper()
	clear := func() {
		for _, key := range keys {
			_, _ = s.pool.Exec(context.Background(),
				`DELETE FROM panel_settings WHERE key = $1`, string(key))
		}
		// panel_audit_log, not panel_audit. It said panel_audit until
		// A5.2 and the error went into the blank that ignores it, so this
		// half of the helper has never once run - the rows it promises to
		// clear have been accumulating since A5.1. Nothing failed because
		// the one test that reads them filters by its own temp-dir path,
		// which is different every run; the point is that a helper whose
		// errors are discarded cannot report that it is not working.
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM panel_audit_log WHERE action = $1`, ActionSettingMigrated)
	}
	clear()
	t.Cleanup(clear)
}

// TestStore_RealDB_MigrationCarriesTheFilesValuesOver is the phase's
// central promise.
//
// A deployment that has been running for a year on tuned numbers must
// still be running on those numbers after the setting moves. The failure
// this guards against is the quiet one: nothing errors, the process
// starts, and the limits are back to the defaults nobody chose.
func TestStore_RealDB_MigrationCarriesTheFilesValuesOver(t *testing.T) {
	s := newTestStore(t, "panel-migrate")
	ctx := context.Background()
	clearMigrated(t, s,
		KeyCollectorMaxConcurrent, KeyCollectorMaxPerSecond,
		KeyCollectorOverloadPolicy, KeyCollectorThrottleQueue)

	path := writeConfig(t, `
site_id = "bir-site"

[limits]
max_concurrent_connections = 4000
max_requests_per_second = 2500
overload_policy = "throttle"
throttle_queue_size = 512
`)

	outcomes, err := s.MigrateSettings(ctx, "collector", path)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}

	want := map[Key]any{
		KeyCollectorMaxConcurrent:  4000,
		KeyCollectorMaxPerSecond:   2500,
		KeyCollectorOverloadPolicy: "throttle",
		KeyCollectorThrottleQueue:  512,
	}
	// Only the keys this file sets. It used to assert that every outcome
	// in the report was written, which held while the collector's
	// catalogue was exactly these four - and broke the moment A5.2 added
	// the [asn_lookup] entries, correctly reporting them as absent from a
	// file that only has a [limits] table. Widening the file to set them
	// too would have kept the loop and lost the test: "the file does not
	// set it" is a case this file is here to produce.
	for _, o := range outcomes {
		if _, mine := want[o.Key]; mine && !o.Written {
			t.Errorf("%s was not written: %s", o.Key, o.Skipped)
		}
	}
	for key, expected := range want {
		got, err := s.GetSetting(ctx, key, "")
		if err != nil {
			t.Fatalf("GetSetting(%s): %v", key, err)
		}
		if got != expected {
			t.Errorf("%s = %v (%T), want %v", key, got, got, expected)
		}
	}

	// The record has to say where each value came from, or a year later
	// nobody can tell a migrated value from one somebody chose.
	entries, _, err := s.Audit(ctx, AuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	// Only this migration's own entries. Asserting that no other
	// setting.migrated row exists anywhere would be asserting something
	// about the rest of the suite rather than about this command.
	filed := map[string]bool{}
	for _, e := range entries {
		if e.Action != ActionSettingMigrated || e.Detail["file"] != path {
			continue
		}
		filed[e.Target] = true
		if e.Detail["from"] == nil || e.Detail["from"] == "" {
			t.Errorf("%s does not name the line it came from", e.Target)
		}
	}
	for key := range want {
		if !filed[string(key)] {
			t.Errorf("no audit entry for %s", key)
		}
	}
}

// TestStore_RealDB_MigrationNeverOverwritesADecision.
//
// The worst thing this command could do, and it would be invisible: a
// value somebody set in the panel, quietly replaced by a line in a file
// they had forgotten about, with the panel going on showing the setting
// as theirs to change.
func TestStore_RealDB_MigrationNeverOverwritesADecision(t *testing.T) {
	s := newTestStore(t, "panel-migrate-keep")
	ctx := context.Background()
	clearMigrated(t, s, KeyCollectorMaxConcurrent)

	// Somebody chose 250 in the panel.
	if err := s.SetSetting(ctx, KeyCollectorMaxConcurrent, "", 250, nil); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	path := writeConfig(t, "[limits]\nmax_concurrent_connections = 9000\n")
	outcomes, err := s.MigrateSettings(ctx, "collector", path)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}

	for _, o := range outcomes {
		if o.Key == KeyCollectorMaxConcurrent {
			if o.Written {
				t.Error("the migration overwrote a value somebody set in the panel")
			}
			if o.Skipped == "" {
				t.Error("the migration skipped it silently; the report has to say why")
			}
		}
	}

	got, err := s.GetSetting(ctx, KeyCollectorMaxConcurrent, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 250 {
		t.Errorf("the setting is now %v; the panel's value was 250", got)
	}
}

// TestStore_RealDB_MigrationIsNotAWayAroundValidation.
//
// A file can hold anything - it was written by hand, possibly years ago,
// possibly against an older build. A migration that wrote whatever it
// found would put values into the table that no reader will accept, and
// the reader's bounds check would then silently substitute a fallback:
// the setting would appear set, in the panel, and do nothing.
func TestStore_RealDB_MigrationIsNotAWayAroundValidation(t *testing.T) {
	s := newTestStore(t, "panel-migrate-junk")
	ctx := context.Background()
	clearMigrated(t, s,
		KeyCollectorMaxConcurrent, KeyCollectorOverloadPolicy, KeyCollectorThrottleQueue)

	path := writeConfig(t, `
[limits]
max_concurrent_connections = 900000
overload_policy = "yavaslat"
throttle_queue_size = 64
`)
	outcomes, err := s.MigrateSettings(ctx, "collector", path)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}

	for _, o := range outcomes {
		switch o.Key {
		case KeyCollectorMaxConcurrent, KeyCollectorOverloadPolicy:
			if o.Written {
				t.Errorf("%s was written despite being out of range or unknown", o.Key)
			}
			if o.Skipped == "" {
				t.Errorf("%s was skipped with no reason given", o.Key)
			}
		case KeyCollectorThrottleQueue:
			// The good value in the same file still moves. A migration
			// that refused the whole file over one bad line would leave
			// an operator editing a file to run a command whose whole
			// purpose is to stop them editing files.
			if !o.Written {
				t.Errorf("a valid value in the same file was not migrated: %s", o.Skipped)
			}
		}
	}
}

// TestStore_RealDB_MigrationSaysWhatTheFileDidNotSet.
//
// "Nothing happened" and "nothing needed to happen" look identical from
// a silent command, and only one of them is fine.
func TestStore_RealDB_MigrationSaysWhatTheFileDidNotSet(t *testing.T) {
	s := newTestStore(t, "panel-migrate-empty")
	ctx := context.Background()
	clearMigrated(t, s, KeyTrustedProxies)

	path := writeConfig(t, "sites = [\"bir-site\"]\n")
	outcomes, err := s.MigrateSettings(ctx, "beacon", path)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}
	if len(outcomes) == 0 {
		t.Fatal("the report is empty, so an operator learns nothing from running this")
	}
	for _, o := range outcomes {
		if o.Written {
			t.Errorf("%s was written from a file that does not set it", o.Key)
		}
		if o.Skipped == "" {
			t.Errorf("%s appears in the report with nothing said about it", o.Key)
		}
	}
}

// TestStore_RealDB_MigrationRefusesAnUnknownService. A typo should be an
// error, not a command that reports having migrated nothing.
func TestStore_RealDB_MigrationRefusesAnUnknownService(t *testing.T) {
	s := newTestStore(t, "panel-migrate-service")
	path := writeConfig(t, "")
	_, err := s.MigrateSettings(context.Background(), "kollektor", path)
	if !errors.Is(err, ErrUnknownService) {
		t.Fatalf("err = %v, want ErrUnknownService", err)
	}
}

// TestStore_RealDB_BlocklistMigratesFromNumbersToText.
//
// A5.2's four keys, and the one place they can go wrong on the way in: a
// config file writes ASNs as TOML numbers (blocked_asns = [64512]) while
// the settings column holds text, so these are the only entries in the
// catalogue whose stored shape differs from the file's. A conversion
// that silently dropped an entry would produce a blocklist shorter than
// the file's, which blocks less than the operator believes and says
// nothing about it.
//
// Migrated together rather than one test each because they come from one
// [asn_lookup] table and a wrong path is the likeliest mistake - reading
// blocked_asns into known_bot_asns would leave both tests green if each
// only checked its own key was non-empty.
func TestStore_RealDB_BlocklistMigratesFromNumbersToText(t *testing.T) {
	s := newTestStore(t, "panel-migrate-blocklist")
	ctx := context.Background()
	clearMigrated(t, s,
		KeyBlockedCountries, KeyBlockedASNs, KeyKnownBotASNs, KeyApplyASNToScoring)

	path := writeConfig(t, `
site_id = "bir-site"

[asn_lookup]
blocked_countries = ["RU", "KP"]
blocked_asns = [64512, 64513]
known_bot_asns = [15169, 32934]
apply_to_scoring = true
`)

	outcomes, err := s.MigrateSettings(ctx, "collector", path)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}
	for _, o := range outcomes {
		switch o.Key {
		case KeyBlockedCountries, KeyBlockedASNs, KeyKnownBotASNs, KeyApplyASNToScoring:
			if !o.Written {
				t.Errorf("%s was not written: %s", o.Key, o.Skipped)
			}
		}
	}

	// Each list asserted whole, in order, against the key it belongs to:
	// the numbers must have become text, and neither ASN list may have
	// picked up the other's entries.
	for key, want := range map[Key][]string{
		KeyBlockedCountries: {"RU", "KP"},
		KeyBlockedASNs:      {"64512", "64513"},
		KeyKnownBotASNs:     {"15169", "32934"},
	} {
		got, err := s.GetSetting(ctx, key, "")
		if err != nil {
			t.Fatalf("GetSetting(%s): %v", key, err)
		}
		list, ok := got.([]string)
		if !ok {
			t.Errorf("%s = %v (%T), want a list of text", key, got, got)
			continue
		}
		if !slices.Equal(list, want) {
			t.Errorf("%s = %v, want %v", key, list, want)
		}
	}

	if got, err := s.GetSetting(ctx, KeyApplyASNToScoring, ""); err != nil {
		t.Fatal(err)
	} else if got != true {
		t.Errorf("%s = %v (%T), want true", KeyApplyASNToScoring, got, got)
	}
}

// TestStore_RealDB_MigrationRefusesAnASNThatWouldMatchEverything.
//
// 0 is asnlookup's "could not resolve", so an ASN 0 rule would block
// every address the lookup failed on rather than one network. The
// validator refuses it; this is here because the conversion above sits
// between the file and that validator, and a conversion is exactly where
// a value can arrive in a shape the validator no longer recognises.
func TestStore_RealDB_MigrationRefusesAnASNThatWouldMatchEverything(t *testing.T) {
	s := newTestStore(t, "panel-migrate-asn-zero")
	ctx := context.Background()
	clearMigrated(t, s, KeyBlockedASNs)

	path := writeConfig(t, "[asn_lookup]\nblocked_asns = [64512, 0]\n")
	outcomes, err := s.MigrateSettings(ctx, "collector", path)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}
	for _, o := range outcomes {
		if o.Key != KeyBlockedASNs {
			continue
		}
		if o.Written {
			t.Error("a blocklist containing AS0 was accepted")
		}
		if o.Skipped == "" {
			t.Error("it was skipped with no reason given")
		}
	}
	// Refused whole, like the proxy list: a partial list blocks less than
	// the file says and nobody would notice which entry went missing.
	got, err := s.GetSetting(ctx, KeyBlockedASNs, "")
	if err != nil {
		t.Fatal(err)
	}
	if list, ok := got.([]string); ok && len(list) != 0 {
		t.Errorf("a partial list was stored: %v", list)
	}
}

// TestStore_RealDB_TrustedProxiesMigratesAsNetworks.
//
// The catalogue's top entry, and the one where a wrong value is worst:
// it does not error, it makes every number derived from the client
// address quietly wrong. So the list is parsed on the way in, and a list
// with one bad entry is refused whole rather than filtered - a partial
// list would trust fewer proxies than the operator believes, which is
// the same failure in a smaller size.
func TestStore_RealDB_TrustedProxiesMigratesAsNetworks(t *testing.T) {
	s := newTestStore(t, "panel-migrate-proxies")
	ctx := context.Background()
	clearMigrated(t, s, KeyTrustedProxies)

	good := writeConfig(t, `trusted_proxies = ["173.245.48.0/20", "2400:cb00::/32", "10.0.0.7"]`+"\n")
	outcomes, err := s.MigrateSettings(ctx, "beacon", good)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}
	for _, o := range outcomes {
		if o.Key == KeyTrustedProxies && !o.Written {
			t.Fatalf("a valid proxy list was not migrated: %s", o.Skipped)
		}
	}

	// And a list with a typo in it is refused, with the reason naming
	// the entry so it can be found in the file.
	clearMigrated(t, s, KeyTrustedProxies)
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM panel_settings WHERE key = $1`, string(KeyTrustedProxies)); err != nil {
		t.Fatal(err)
	}
	bad := writeConfig(t, `trusted_proxies = ["173.245.48.0/20", "173.245.48.0/twenty"]`+"\n")
	outcomes, err = s.MigrateSettings(ctx, "beacon", bad)
	if err != nil {
		t.Fatalf("MigrateSettings: %v", err)
	}
	for _, o := range outcomes {
		if o.Key != KeyTrustedProxies {
			continue
		}
		if o.Written {
			t.Error("a proxy list with an entry that is not a network was accepted")
		}
		if o.Skipped == "" {
			t.Error("it was skipped with no reason given")
		}
	}
	got, err := s.GetSetting(ctx, KeyTrustedProxies, "")
	if err != nil {
		t.Fatal(err)
	}
	if list, ok := got.([]string); ok && len(list) != 0 {
		t.Errorf("a partial list was stored: %v", list)
	}
}
