//go:build integration

// Real end-to-end coverage against a live TimescaleDB, gated behind the
// "integration" build tag so `go test ./...` (no tags) - which must stay
// runnable with no external dependencies - never touches it. Previously
// this pgx/netip.Addr <-> inet round trip was only verified by reading
// pgx's source, since no Docker daemon was available when this package
// was first written. Run it with:
//
//	docker compose up -d
//	docker compose ps --format '{{.Name}}: {{.Health}}' # wait for healthy
//	go test -tags integration ./internal/storage/... -run TestWriter_RealTimescaleDB -v

package storage

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

const testDatabaseURL = "postgres://collector:collector@localhost:5432/analytics"

func TestWriter_RealTimescaleDB_WriteAndReadBack(t *testing.T) {
	ctx := context.Background()
	w, err := NewWriter(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("NewWriter: %v (is `docker compose up -d` running, with schema.sql applied?)", err)
	}
	// t.Cleanup, not defer: cleanups run in LIFO order relative to each
	// other, but only *after* the test function (and its own defers) has
	// already returned - a plain `defer w.Close()` here would close the
	// pool before a t.Cleanup-registered delete ever got to run against
	// it. Registering Close() first and the delete second means the
	// delete (last registered) runs first, against a still-open pool.
	t.Cleanup(w.Close)

	ip := netip.MustParseAddr("203.0.113.42")
	// Truncate to microseconds: that's what a timestamptz column actually
	// stores, and Go's monotonic reading + nanosecond precision would
	// otherwise make the read-back value compare unequal to what was sent.
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := Row{
		Time:            now,
		SiteID:          "integration-test-site",
		IP:              ip,
		JA4:             "t13d1516h2_integration_test",
		PrevWindowCount: 7,
		CurrWindowCount: 3,
		RequestRate:     1.5,
		BotScore:        42,
		IsKnownBotJA4:   true,
		Country:         "US",
		ASN:             64512,
		ASNName:         "Example Org",
		IsKnownBotASN:   true,
	}
	t.Cleanup(func() {
		if _, err := w.pool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE ja4 = $1`, row.JA4); err != nil {
			t.Logf("cleanup: delete failed: %v", err)
		}
	})

	n, err := w.WriteRows(ctx, []Row{row})
	if err != nil {
		t.Fatalf("WriteRows: %v", err)
	}
	if n != 1 {
		t.Fatalf("WriteRows returned %d, want 1", n)
	}

	// Read back through a completely separate path from the one that
	// wrote it (a raw query, not WriteRows/pgx.CopyFrom) so this actually
	// exercises pgx's decode direction for netip.Addr <-> inet, not just
	// its encode direction.
	var gotTime time.Time
	var gotIP netip.Addr
	var gotJA4 string
	var gotPrev, gotCurr int
	var gotRate float64
	var gotScore int16
	var gotKnownBot bool
	var gotCountry string
	var gotASN int
	var gotASNName string
	var gotKnownBotASN bool
	var gotSiteID string
	// ORDER BY ... DESC LIMIT 1 rather than a bare WHERE: defense in depth
	// against exactly the leftover-row bug the cleanup fix above addresses
	// - if any previous run's cleanup ever fails for some other reason,
	// this still reads back the row this run just wrote, not a stale one.
	err = w.pool.QueryRow(ctx,
		`SELECT time, site_id, ip, ja4, prev_window_count, curr_window_count, request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org, is_known_bot_asn
		 FROM traffic_snapshots WHERE ja4 = $1 ORDER BY time DESC LIMIT 1`,
		row.JA4,
	).Scan(&gotTime, &gotSiteID, &gotIP, &gotJA4, &gotPrev, &gotCurr, &gotRate, &gotScore, &gotKnownBot, &gotCountry, &gotASN, &gotASNName, &gotKnownBotASN)
	if err != nil {
		t.Fatalf("query back: %v", err)
	}

	if gotSiteID != row.SiteID {
		t.Errorf("read back SiteID = %q, want %q", gotSiteID, row.SiteID)
	}

	if !gotTime.Equal(row.Time) {
		t.Errorf("read back Time = %v, want %v", gotTime, row.Time)
	}
	if gotIP != row.IP {
		t.Errorf("read back IP = %v, want %v", gotIP, row.IP)
	}
	if gotJA4 != row.JA4 {
		t.Errorf("read back JA4 = %q, want %q", gotJA4, row.JA4)
	}
	if gotPrev != row.PrevWindowCount || gotCurr != row.CurrWindowCount {
		t.Errorf("read back window counts = (%d, %d), want (%d, %d)", gotPrev, gotCurr, row.PrevWindowCount, row.CurrWindowCount)
	}
	if gotRate != row.RequestRate {
		t.Errorf("read back RequestRate = %v, want %v", gotRate, row.RequestRate)
	}
	if gotScore != row.BotScore {
		t.Errorf("read back BotScore = %d, want %d", gotScore, row.BotScore)
	}
	if gotKnownBot != row.IsKnownBotJA4 {
		t.Errorf("read back IsKnownBotJA4 = %v, want %v", gotKnownBot, row.IsKnownBotJA4)
	}
	if gotCountry != row.Country || gotASN != row.ASN || gotASNName != row.ASNName {
		t.Errorf("read back Country/ASN/ASNName = %q/%d/%q, want %q/%d/%q", gotCountry, gotASN, gotASNName, row.Country, row.ASN, row.ASNName)
	}
	if gotKnownBotASN != row.IsKnownBotASN {
		t.Errorf("read back IsKnownBotASN = %v, want %v", gotKnownBotASN, row.IsKnownBotASN)
	}
}

// TestWriter_RealTimescaleDB_TwoSitesStaySeparable is the actual point of
// site_id: one VDS hosting two customer sites runs two collectors against
// one database, and each site's rows must stay cleanly filterable. Writing
// both sites' rows for the *same* IP at the *same* timestamp is
// deliberate - it's the case where every other column collides, so only
// site_id can tell them apart.
func TestWriter_RealTimescaleDB_TwoSitesStaySeparable(t *testing.T) {
	ctx := context.Background()
	w, err := NewWriter(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(w.Close)

	const ja4 = "t13d1516h2_integration_test_two_sites"
	now := time.Now().UTC().Truncate(time.Microsecond)
	ip := netip.MustParseAddr("203.0.113.44")
	rows := []Row{
		{Time: now, SiteID: "site-a", IP: ip, JA4: ja4, BotScore: 10},
		{Time: now, SiteID: "site-b", IP: ip, JA4: ja4, BotScore: 20},
	}
	t.Cleanup(func() {
		if _, err := w.pool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE ja4 = $1`, ja4); err != nil {
			t.Logf("cleanup: delete failed: %v", err)
		}
	})

	if _, err := w.WriteRows(ctx, rows); err != nil {
		t.Fatalf("WriteRows: %v", err)
	}

	for _, want := range rows {
		var gotScore int16
		var count int
		err := w.pool.QueryRow(ctx,
			`SELECT count(*), max(bot_score) FROM traffic_snapshots WHERE ja4 = $1 AND site_id = $2`,
			ja4, want.SiteID,
		).Scan(&count, &gotScore)
		if err != nil {
			t.Fatalf("query back for %s: %v", want.SiteID, err)
		}
		if count != 1 {
			t.Errorf("site %s: got %d rows, want exactly 1 (site_id must isolate each site's rows)", want.SiteID, count)
		}
		if gotScore != want.BotScore {
			t.Errorf("site %s: bot_score = %d, want %d (rows must not be mixed up between sites)", want.SiteID, gotScore, want.BotScore)
		}
	}
}

// TestWriter_RealTimescaleDB_GeoColumnsDefaultToZeroValue confirms a Row
// that leaves Country/ASN/ASNName/IsKnownBotASN unset (the shape every row
// has when asn_lookup.enabled = false, i.e. what BuildRows produces with a
// nil resolver) round-trips through the real COPY pipeline as empty
// string/zero/false, not e.g. NULL or some other pgx encoding quirk for an
// empty TEXT/zero INTEGER. Note this doesn't exercise schema.sql's own
// DEFAULT clauses - WriteRows's CopyFrom always sends every column in
// the columns list explicitly, so those defaults only matter for a
// hand-written INSERT that omits the column, never for this write path.
func TestWriter_RealTimescaleDB_GeoColumnsDefaultToZeroValue(t *testing.T) {
	ctx := context.Background()
	w, err := NewWriter(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(w.Close)

	now := time.Now().UTC().Truncate(time.Microsecond)
	row := Row{
		Time: now,
		IP:   netip.MustParseAddr("203.0.113.43"),
		JA4:  "t13d1516h2_integration_test_no_geo",
	}
	t.Cleanup(func() {
		if _, err := w.pool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE ja4 = $1`, row.JA4); err != nil {
			t.Logf("cleanup: delete failed: %v", err)
		}
	})

	if _, err := w.WriteRows(ctx, []Row{row}); err != nil {
		t.Fatalf("WriteRows: %v", err)
	}

	var gotCountry string
	var gotASN int
	var gotASNName string
	var gotKnownBotASN bool
	err = w.pool.QueryRow(ctx,
		`SELECT country, asn, asn_org, is_known_bot_asn FROM traffic_snapshots WHERE ja4 = $1 ORDER BY time DESC LIMIT 1`,
		row.JA4,
	).Scan(&gotCountry, &gotASN, &gotASNName, &gotKnownBotASN)
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	if gotCountry != "" || gotASN != 0 || gotASNName != "" || gotKnownBotASN {
		t.Errorf("read back Country/ASN/ASNName/IsKnownBotASN = %q/%d/%q/%v, want all zero value", gotCountry, gotASN, gotASNName, gotKnownBotASN)
	}
}

// TestWriter_RealTimescaleDB_HypertableExists confirms schema.sql's
// create_hypertable call actually took effect - a plain CREATE TABLE
// would happily accept the same INSERTs and this test would still pass,
// which is exactly why it's worth asserting on directly rather than
// trusting the schema file was applied as intended.
func TestWriter_RealTimescaleDB_HypertableExists(t *testing.T) {
	ctx := context.Background()
	w, err := NewWriter(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	var count int
	err = w.pool.QueryRow(ctx,
		`SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name = 'traffic_snapshots'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query timescaledb_information.hypertables: %v", err)
	}
	if count != 1 {
		t.Errorf("traffic_snapshots hypertable count = %d, want 1 (schema.sql's create_hypertable call)", count)
	}
}
