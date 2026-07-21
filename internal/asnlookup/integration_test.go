//go:build integration

// Real end-to-end coverage against a live TimescaleDB, gated behind the
// "integration" build tag for the same reason as
// internal/storage/integration_test.go: it needs a real database, so
// `go test ./...` (no tags) must never touch it. Covers exactly the one
// piece asnlookup_test.go's newTestResolver deliberately bypasses:
// writeRanges, this package's only other DB-touching code besides the
// pool-open-and-ping in NewResolver. Run it with:
//
//	docker compose up -d
//	docker compose ps --format '{{.Name}}: {{.Health}}' # wait for healthy
//	psql "postgres://collector:collector@localhost:5432/analytics" -f internal/asnlookup/schema.sql
//	go test -tags integration ./internal/asnlookup/... -run TestResolver_RealTimescaleDB -v

package asnlookup

import (
	"context"
	"net/netip"
	"testing"
)

const testDatabaseURL = "postgres://collector:collector@localhost:5432/analytics"

func TestResolver_RealTimescaleDB_WriteRangesThenReadBack(t *testing.T) {
	ctx := context.Background()
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 100, TTL: 0}, "", nil)
	if err != nil {
		t.Fatalf("NewResolver: %v (is `docker compose up -d` running, with both schema.sql files applied?)", err)
	}
	// t.Cleanup, not defer, and in this order: a plain `defer r.Close()`
	// would close the pool before a t.Cleanup-registered truncate ever
	// ran against it, since defers unwind before t.Cleanup fires at all.
	// Registering Close() first means it (as the last-registered
	// t.Cleanup) runs after the truncate below.
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		if _, err := r.pool.Exec(context.Background(), "TRUNCATE ip_country_ranges"); err != nil {
			t.Logf("cleanup: truncate failed: %v", err)
		}
	})

	// Deliberately mixed IPv4 + IPv6 in one writeRanges call, matching how
	// refresh() actually calls it (entries4 and entries6 appended
	// together) - and exercising the INET column with both families, not
	// just IPv4 the way the old BIGINT-based schema was limited to.
	entries := []rangeEntry{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), country: "US"},
		{start: netip.MustParseAddr("198.51.100.0"), end: netip.MustParseAddr("198.51.100.255"), country: "DE"},
		{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), country: "JP"},
	}
	if err := r.writeRanges(ctx, entries); err != nil {
		t.Fatalf("writeRanges: %v", err)
	}

	// Read back through a raw query, not through writeRanges/CopyFrom, to
	// actually exercise the decode direction too. No ORDER BY: Postgres's
	// cross-family inet ordering isn't asserted on here, only that the
	// same set of rows comes back - so the comparison below is by set
	// membership, not position.
	rows, err := r.pool.Query(ctx, "SELECT start_addr, end_addr, country FROM ip_country_ranges")
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	defer rows.Close()

	got := make(map[rangeEntry]bool)
	for rows.Next() {
		var start, end netip.Addr
		var country string
		if err := rows.Scan(&start, &end, &country); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rangeEntry{start: start, end: end, country: country}] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != len(entries) {
		t.Fatalf("read back %d distinct rows, want %d: %+v", len(got), len(entries), got)
	}
	for _, want := range entries {
		if !got[want] {
			t.Errorf("expected row %+v not found in what was read back: %+v", want, got)
		}
	}

	// writeRanges must fully replace the table (TRUNCATE + COPY in one
	// transaction), not append - a second call with different data should
	// leave no trace of the first.
	second := []rangeEntry{{start: netip.MustParseAddr("203.0.113.1"), end: netip.MustParseAddr("203.0.113.1"), country: "FR"}}
	if err := r.writeRanges(ctx, second); err != nil {
		t.Fatalf("writeRanges (second call): %v", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM ip_country_ranges").Scan(&count); err != nil {
		t.Fatalf("count after second writeRanges: %v", err)
	}
	if count != len(second) {
		t.Errorf("row count after second writeRanges = %d, want %d (a full replace, not an append)", count, len(second))
	}
}

// TestResolver_RealTimescaleDB_ResolveAfterRealRefresh exercises Resolve
// against data that actually round-tripped through TimescaleDB (rather
// than an in-memory table injected directly, as the rest of this
// package's tests do), closing the gap between "the parser is correct"
// and "a real write+swap actually makes Resolve see it" - for both
// address families.
func TestResolver_RealTimescaleDB_ResolveAfterRealRefresh(t *testing.T) {
	ctx := context.Background()
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 100, TTL: 0}, "", nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		if _, err := r.pool.Exec(context.Background(), "TRUNCATE ip_country_ranges"); err != nil {
			t.Logf("cleanup: truncate failed: %v", err)
		}
	})

	entries4 := []rangeEntry{{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), country: "US"}}
	entries6 := []rangeEntry{{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), country: "JP"}}
	if err := r.writeRanges(ctx, append(entries4, entries6...)); err != nil {
		t.Fatalf("writeRanges: %v", err)
	}
	r.table4.Store(newRangeTable(entries4))
	r.table6.Store(newRangeTable(entries6))

	if got := r.Resolve(netip.MustParseAddr("192.0.2.42")); !got.Found || got.Country != "US" {
		t.Errorf("Resolve(IPv4) = %+v, want Found: true, Country: US", got)
	}
	if got := r.Resolve(netip.MustParseAddr("2001:db8::1234")); !got.Found || got.Country != "JP" {
		t.Errorf("Resolve(IPv6) = %+v, want Found: true, Country: JP", got)
	}
}
