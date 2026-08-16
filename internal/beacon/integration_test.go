//go:build integration

// Real coverage of the beacon's write path against a live TimescaleDB,
// gated behind the "integration" build tag like internal/storage's and
// internal/api's. These exercise what server_test.go's fake sink
// deliberately cannot: that the COPY column list matches the schema,
// that hostile field contents survive a batch write rather than
// poisoning it, and that the buffer's shutdown drain really does write
// what it was holding. Run with:
//
//	docker compose up -d
//	psql "postgres://collector:collector@localhost:5432/analytics" -f internal/beacon/schema.sql
//	go test -tags integration ./internal/beacon/... -v

package beacon

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURL = "postgres://collector:collector@localhost:5432/analytics"

// newTestWriter opens a Writer against the real database and deletes
// everything it wrote for siteID afterwards. Every test uses a site_id
// unique to itself, so cleanup can be exact without disturbing anything
// else in the database.
func newTestWriter(t *testing.T, siteID string, cfg WriterConfig) *Writer {
	t.Helper()

	writer, err := NewWriter(context.Background(), testDatabaseURL, cfg)
	if err != nil {
		t.Fatalf("NewWriter: %v (is docker compose up and the schema applied?)", err)
	}
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), testDatabaseURL)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(context.Background(), `DELETE FROM beacon_events WHERE site_id = $1`, siteID); err != nil {
			t.Logf("cleanup: %v", err)
		}
		writer.Close()
	})
	return writer
}

func countRows(t *testing.T, siteID string) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM beacon_events WHERE site_id = $1`, siteID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func testRow(siteID, path string) Row {
	return Row{
		Time:         time.Now().UTC(),
		SiteID:       siteID,
		VisitorID:    "0123456789abcdef0123456789abcdef",
		EventType:    TypePageview,
		Path:         path,
		ReferrerHost: "google.com",
		ReferrerPath: "/search",
		IP:           netip.MustParseAddr("203.0.113.9"),
		Browser:      "Chrome",
		OS:           "Windows",
		Device:       DeviceDesktop,
		ScreenW:      1920,
		ScreenH:      1080,
		Language:     "tr-TR",
		Country:      "TR",
		ASN:          9121,
		ASNOrg:       "TURKTELEKOM",
	}
}

// The COPY column list is hand-maintained and must match schema.sql
// exactly. Nothing but a real write catches a drift between them.
func TestWriter_RealTimescaleDB_WritesEveryColumn(t *testing.T) {
	site := "beacon-columns-test"
	writer := newTestWriter(t, site, WriterConfig{})

	want := testRow(site, "/pricing")
	want.EventType = TypeEvent
	want.EventName = "signup"
	want.Query = "utm_source=newsletter"
	want.Title = "Fiyatlar"
	want.IsBotUA = true

	if _, err := writer.WriteRows(context.Background(), []Row{want}); err != nil {
		t.Fatalf("WriteRows: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var got Row
	err = pool.QueryRow(context.Background(), `
		SELECT time, site_id, visitor_id, event_type, event_name,
		       path, query, title, referrer_host, referrer_path,
		       ip, browser, os, device, is_bot_ua,
		       screen_w, screen_h, language, country, asn, asn_org
		FROM beacon_events WHERE site_id = $1`, site).Scan(
		&got.Time, &got.SiteID, &got.VisitorID, &got.EventType, &got.EventName,
		&got.Path, &got.Query, &got.Title, &got.ReferrerHost, &got.ReferrerPath,
		&got.IP, &got.Browser, &got.OS, &got.Device, &got.IsBotUA,
		&got.ScreenW, &got.ScreenH, &got.Language, &got.Country, &got.ASN, &got.ASNOrg)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Compared field by field rather than as whole structs, since the
	// database round-trips the timestamp at microsecond precision.
	if !got.Time.Equal(want.Time.Truncate(time.Microsecond)) {
		t.Errorf("Time = %v, want %v", got.Time, want.Time)
	}
	got.Time, want.Time = time.Time{}, time.Time{}
	// reflect.DeepEqual rather than !=: Row carries the ip_hash byte
	// slice now, and a slice makes the struct uncomparable.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the row:\n got %+v\nwant %+v", got, want)
	}
}

// PostgreSQL rejects a NUL byte or invalid UTF-8 in a TEXT column and
// fails the entire statement. Because rows are written in batches, one
// hostile payload landing in a batch would otherwise destroy every
// other visitor's events in it. This test drives a real, hostile
// payload through the real HTTP handler into a real COPY alongside
// innocent rows, which is the only way to prove the sanitizing in
// event.go is actually sufficient for the database.
func TestWriter_RealTimescaleDB_HostilePayloadDoesNotPoisonTheBatch(t *testing.T) {
	site := "beacon-poison-test"
	writer := newTestWriter(t, site, WriterConfig{})

	hostile := BuildRow(Event{
		Site:  site,
		Type:  TypeEvent,
		Name:  "nul\x00byte",
		URL:   "/path\x00with/nul",
		Title: "invalid \xff utf8 and a \x00 nul and a newline\n",
	}, Enrichment{
		Time:      time.Now().UTC(),
		IP:        netip.MustParseAddr("203.0.113.9"),
		VisitorID: "0123456789abcdef0123456789abcdef",
	}, DefaultCampaignPolicy())

	batch := []Row{testRow(site, "/before"), hostile, testRow(site, "/after")}
	n, err := writer.WriteRows(context.Background(), batch)
	if err != nil {
		t.Fatalf("a hostile payload failed the whole batch: %v", err)
	}
	if n != 3 {
		t.Errorf("wrote %d rows, want 3", n)
	}
	if got := countRows(t, site); got != 3 {
		t.Errorf("%d rows in the database, want 3 - the innocent rows were lost with the hostile one", got)
	}
}

func TestWriter_RealTimescaleDB_BufferFlushesOnTheInterval(t *testing.T) {
	site := "beacon-interval-test"
	writer := newTestWriter(t, site, WriterConfig{FlushInterval: 100 * time.Millisecond, BatchSize: 1000})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); writer.Run(ctx) }()

	if !writer.Enqueue(testRow(site, "/one")) {
		t.Fatal("Enqueue reported a full buffer on an empty one")
	}

	// Well under BatchSize, so only the interval can flush this.
	deadline := time.Now().Add(5 * time.Second)
	for countRows(t, site) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the buffered row was never flushed by the interval")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done
}

// A clean shutdown must not lose buffered events - that is the whole
// reason Run has a drain phase, and the reason main.go cancels the
// writer's context only after the server has stopped accepting.
func TestWriter_RealTimescaleDB_ShutdownDrainsTheBuffer(t *testing.T) {
	site := "beacon-drain-test"
	// A long interval and a large batch, so nothing can flush before the
	// drain: if these rows land, they landed because of the drain.
	writer := newTestWriter(t, site, WriterConfig{FlushInterval: time.Hour, BatchSize: 10_000})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); writer.Run(ctx) }()

	const enqueued = 25
	for i := range enqueued {
		if !writer.Enqueue(testRow(site, fmt.Sprintf("/page-%d", i))) {
			t.Fatalf("Enqueue %d reported a full buffer", i)
		}
	}

	if got := countRows(t, site); got != 0 {
		t.Fatalf("%d rows landed before shutdown; the test cannot prove the drain works", got)
	}

	cancel()
	<-done

	if got := countRows(t, site); got != enqueued {
		t.Errorf("%d rows after the drain, want %d", got, enqueued)
	}
	if written, dropped := writer.Counters(); written != enqueued || dropped != 0 {
		t.Errorf("counters = written %d, dropped %d; want %d and 0", written, dropped, enqueued)
	}
}

// Enqueue must never block: blocking would push database backpressure
// out to a visitor's browser, which is exactly backwards.
func TestWriter_RealTimescaleDB_FullBufferDropsRatherThanBlocks(t *testing.T) {
	site := "beacon-fullbuffer-test"
	// A tiny buffer, and no Run goroutine to consume it.
	writer := newTestWriter(t, site, WriterConfig{BufferSize: 4, BatchSize: 2, FlushInterval: time.Hour})

	accepted := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			if writer.Enqueue(testRow(site, "/x")) {
				accepted++
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked on a full buffer")
	}

	if accepted != 4 {
		t.Errorf("accepted %d rows, want exactly the buffer size (4)", accepted)
	}
	if _, dropped := writer.Counters(); dropped != 96 {
		t.Errorf("dropped = %d, want 96", dropped)
	}
}

// The join back to traffic_snapshots by IP is the entire reason both
// data sources share one database: it answers "did this IP also run
// JavaScript?", which neither table can answer alone.
func TestWriter_RealTimescaleDB_JoinsToTrafficSnapshotsByIP(t *testing.T) {
	site := "beacon-join-test"
	writer := newTestWriter(t, site, WriterConfig{})

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	// Two IPs reached the collector; only one of them ran JavaScript.
	ranJS := netip.MustParseAddr("203.0.113.50")
	silent := netip.MustParseAddr("203.0.113.51")
	for _, ip := range []netip.Addr{ranJS, silent} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count, request_rate, bot_score)
			VALUES ($1, $2, $3, 'x', 1, 1, 1.0, 10)`, now, site, ip); err != nil {
			t.Fatalf("seed traffic_snapshots: %v", err)
		}
	}
	t.Cleanup(func() {
		// A fresh pool: the one above is closed by this function's own
		// defer, which runs before any t.Cleanup does.
		cleanupPool, err := pgxpool.New(context.Background(), testDatabaseURL)
		if err != nil {
			t.Logf("cleanup traffic_snapshots: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE site_id = $1`, site); err != nil {
			t.Logf("cleanup traffic_snapshots: %v", err)
		}
	})

	beaconRow := testRow(site, "/landing")
	beaconRow.IP = ranJS
	if _, err := writer.WriteRows(ctx, []Row{beaconRow}); err != nil {
		t.Fatalf("WriteRows: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT t.ip, count(b.time) > 0 AS ran_js
		FROM traffic_snapshots t
		LEFT JOIN beacon_events b ON b.ip = t.ip AND b.site_id = t.site_id
		WHERE t.site_id = $1
		GROUP BY t.ip`, site)
	if err != nil {
		t.Fatalf("join query: %v", err)
	}
	defer rows.Close()

	ranJSBy := map[netip.Addr]bool{}
	for rows.Next() {
		var ip netip.Addr
		var didRun bool
		if err := rows.Scan(&ip, &didRun); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ranJSBy[ip] = didRun
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(ranJSBy) != 2 {
		t.Fatalf("joined %d IPs, want 2", len(ranJSBy))
	}
	if !ranJSBy[ranJS] {
		t.Error("the IP that sent a beacon event was not matched by the join")
	}
	if ranJSBy[silent] {
		t.Error("an IP that never sent a beacon event was reported as having run JavaScript")
	}
}

func TestWriter_RealTimescaleDB_StoresIPv6(t *testing.T) {
	site := "beacon-ipv6-test"
	writer := newTestWriter(t, site, WriterConfig{})

	want := testRow(site, "/v6")
	want.IP = netip.MustParseAddr("2001:db8:abcd:1234::1")
	if _, err := writer.WriteRows(context.Background(), []Row{want}); err != nil {
		t.Fatalf("WriteRows: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var got netip.Addr
	if err := pool.QueryRow(context.Background(), `SELECT ip FROM beacon_events WHERE site_id = $1`, site).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != want.IP {
		t.Errorf("ip = %v, want %v", got, want.IP)
	}
}

func TestWriter_RealTimescaleDB_StoresLongButValidText(t *testing.T) {
	// Truncation happens in BuildRow, not the database, so a row built
	// from a maximal payload must still fit every column.
	site := "beacon-longtext-test"
	writer := newTestWriter(t, site, WriterConfig{})

	row := BuildRow(Event{
		Site:     site,
		Type:     TypeEvent,
		Name:     strings.Repeat("ş", 500),
		URL:      "/" + strings.Repeat("ğ", 2000) + "?utm_source=" + strings.Repeat("ü", 2000),
		Title:    strings.Repeat("ç", 2000),
		Referrer: "https://" + strings.Repeat("a", 400) + ".example/" + strings.Repeat("b", 2000),
		Language: strings.Repeat("x", 200),
	}, Enrichment{
		Time:      time.Now().UTC(),
		IP:        netip.MustParseAddr("203.0.113.9"),
		VisitorID: "0123456789abcdef0123456789abcdef",
	}, DefaultCampaignPolicy())

	if _, err := writer.WriteRows(context.Background(), []Row{row}); err != nil {
		t.Fatalf("WriteRows on a maximal row: %v", err)
	}
	if got := countRows(t, site); got != 1 {
		t.Errorf("%d rows, want 1", got)
	}
}
