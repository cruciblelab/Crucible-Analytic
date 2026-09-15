//go:build integration

// P5a: does a mode chosen in the panel change what the collector writes?
//
// Against a real settings table and a real traffic_snapshots, because
// the claim spans three layers - a row the panel inserts, the config
// package that resolves it, and the column the writer fills - and a
// stand-in for any one of them would move the measurement off the thing
// that was broken.
//
// # What was broken
//
// Nothing here applied privacy.ip_storage. The beacon had taken it live
// since A6; this process read it from its file at startup and never
// again, so one click in the panel moved one writer of the crossover
// join and left the other where it was. The two encodings never compare
// equal, so the two tables shared no keys at all: coverage 0%, every
// beacon address in beacon_only_ips, and a panel explaining that as "the
// collector is not in the path".
//
// See collector.PrivacyConfig.Live for the direction of it that reaches
// a visitor, which is the worse one.

package storage

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The key is long enough for privacy.MinHashKeyLen; its content does not
// matter to any assertion here, only that a token can be produced.
const liveModeKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// storeIPMode writes the deployment-wide row the way the panel does.
//
// Raw SQL rather than through internal/panel, for the reason the
// beacon's copy of this helper gives: the panel's data layer has no
// business inside a service on the traffic path, and importing it to
// save five lines would quietly undo a dependency direction that is a
// property rather than a habit.
func storeIPMode(t *testing.T, mode string) {
	t.Helper()
	pool := testdb.Pool(t, testdb.Panel)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, $2)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		settings.KeyPrivacyIPStorage, `"`+mode+`"`,
	); err != nil {
		t.Fatalf("storing %s = %s: %v", settings.KeyPrivacyIPStorage, mode, err)
	}
}

// clearIPMode removes the row, whatever this test left in it.
//
// Registered by the caller rather than inside storeIPMode: a suite that
// writes the row twice would otherwise register two cleanups, and the
// row this test must not leave behind is one row.
func clearIPMode(t *testing.T) {
	t.Helper()
	pool := testdb.Pool(t, testdb.Panel)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key = $1 AND site_id = ''`,
			settings.KeyPrivacyIPStorage); err != nil {
			t.Logf("cleanup: deleting %s: %v", settings.KeyPrivacyIPStorage, err)
		}
	})
}

// liveMode resolves the mode the way cmd/collector does, from a source
// that has just read the database.
func liveMode(t *testing.T, pool *pgxpool.Pool, fileValue string) privacy.IPMode {
	t.Helper()
	src := settings.New(context.Background(), pool, settings.Config{Interval: time.Minute})
	if !src.Loaded() {
		t.Fatal("the settings source never loaded; is the panel schema applied?")
	}
	return collector.PrivacyConfig{IPStorage: fileValue, IPHashKey: liveModeKey}.Live(src)
}

// A mode stored by the panel decides what the next flush writes.
//
// Both directions, and the pair is the assertion: a test that only
// raised the mode would pass against a writer that tokenised
// unconditionally, and one that only lowered it would pass against a
// writer that never tokenised at all. What is being measured is that the
// column follows the setting, which needs the setting to move.
func TestTheStoredModeDecidesWhatTheCollectorWrites(t *testing.T) {
	// Shared with internal/beacon's livesettings suite, which writes the
	// same deployment-wide row. Both suites run against one database and
	// Go runs packages in parallel, so without this one of them reads
	// the other's value and fails describing a product defect.
	testdb.Lock(t, testdb.Admin(t), testdb.IPModeSettingLock)
	clearIPMode(t)

	ctx := context.Background()
	w, err := NewWriter(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("NewWriter: %v (is the database up and installed? see internal/testdb)", err)
	}
	t.Cleanup(w.Close)

	// The collector's own role reads the settings table, because that is
	// the pool cmd/collector hands to settings.New - the point of these
	// tests is that one role writes the row and a different one, with
	// different privileges, picks it up.
	settingsPool := testdb.Pool(t, testdb.Collector)

	const ja4 = "p5a-live-ip-mode"
	clearJA4(t, ja4)

	// The file says masked - the state a deployment upgrades in, and the
	// state that made the defect invisible: with the file and the panel
	// agreeing, nothing distinguishes a process that reads the panel
	// from one that ignores it.
	flush := func(mode privacy.IPMode) Row {
		t.Helper()
		store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
		defer store.Close()
		store.RecordRequest(netip.MustParseAddr("203.0.113.42"), ja4, time.Now())

		f := &Flusher{
			Store:     store,
			SiteID:    "p5a",
			Writer:    w,
			Interval:  time.Hour,
			IPMode:    privacy.IPMasked,
			IPHashKey: []byte(liveModeKey),
		}
		f.SetIPMode(mode)

		f.flushOnce(ctx, time.Time{}, time.Now())

		var (
			ip     netip.Addr
			hash   []byte
			hashed bool
		)
		if err := w.Pool().QueryRow(ctx, `
			SELECT ip, COALESCE(ip_hash, ''::bytea), ip_hash IS NOT NULL
			FROM traffic_snapshots WHERE ja4 = $1 ORDER BY time DESC LIMIT 1`,
			ja4,
		).Scan(&ip, &hash, &hashed); err != nil {
			t.Fatalf("reading back the row the flusher wrote: %v", err)
		}
		if !hashed {
			hash = nil
		}
		return Row{IP: ip, IPHash: hash}
	}

	// ---- nothing stored: the file decides ----
	if got := liveMode(t, settingsPool, string(privacy.IPMasked)); got != privacy.IPMasked {
		t.Fatalf("with nothing stored the mode is %q; the file's value must survive", got)
	}
	if row := flush(liveMode(t, settingsPool, string(privacy.IPMasked))); row.IPHash != nil {
		t.Errorf("masked mode wrote a %d-byte token; the column must be NULL", len(row.IPHash))
	}

	// ---- stored as full: the panel decides ----
	storeIPMode(t, string(privacy.IPFull))
	if got := liveMode(t, settingsPool, string(privacy.IPMasked)); got != privacy.IPFull {
		t.Fatalf("the stored mode is %q and the file says masked; the panel's value must win", got)
	}
	full := flush(liveMode(t, settingsPool, string(privacy.IPMasked)))
	if full.IPHash == nil {
		t.Fatal("full mode wrote no token, so the setting never reached the column. " +
			"This is the defect P5a fixed: the panel shows privacy.ip_storage as live, " +
			"the beacon honours it, and the collector read its file and nothing else.")
	}
	if want := privacy.TokenIP(netip.MustParseAddr("203.0.113.42"), []byte(liveModeKey)); string(full.IPHash) != string(want) {
		t.Errorf("the token is %x; the whole address keyed with the configured key is %x",
			full.IPHash, want)
	}
	// The masked network is written in both modes - full mode adds the
	// token beside it rather than replacing it, which is why the
	// crossover join never has to choose between two shapes of address.
	if got := full.IP.String(); got != "203.0.113.0" {
		t.Errorf("full mode stored %s; every mode stores the masked network", got)
	}

	// ---- stored back to masked: the panel decides again ----
	//
	// The direction that reaches a visitor. A deployment installed in
	// full mode whose customer selects masked gets a beacon that stops
	// tokenising; before this fix the collector did not, so
	// traffic_snapshots.ip_hash kept being written under a disclosure
	// page saying only the masked network was kept.
	storeIPMode(t, string(privacy.IPMasked))
	if got := liveMode(t, settingsPool, string(privacy.IPFull)); got != privacy.IPMasked {
		t.Fatalf("stored masked over a file saying full resolved to %q", got)
	}
	if row := flush(liveMode(t, settingsPool, string(privacy.IPFull))); row.IPHash != nil {
		t.Errorf("after the panel lowered the mode the collector still wrote a token (%x). "+
			"The disclosure page derives from the beacon, which stopped - so this row is "+
			"personal data the visitor-facing page says is not kept.", row.IPHash)
	}
}
