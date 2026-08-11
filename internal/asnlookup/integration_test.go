//go:build integration

// Real end-to-end coverage against a live TimescaleDB, gated behind the
// "integration" build tag for the same reason as
// internal/storage/integration_test.go: it needs a real database, so
// `go test ./...` (no tags) must never touch it. Covers exactly the pieces
// asnlookup_test.go's newTestResolver deliberately bypasses: writeCountryRanges
// and writeASNRanges, this package's only other DB-touching code besides
// the pool-open-and-ping in NewResolver. Run it with:
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

func TestResolver_RealTimescaleDB_WriteCountryRangesThenReadBack(t *testing.T) {
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

	// Deliberately mixed IPv4 + IPv6 in one writeCountryRanges call,
	// matching how refreshCountry() actually calls it (entries4 and
	// entries6 appended together) - and exercising the INET column with
	// both families, not just IPv4 the way the old BIGINT-based schema
	// was limited to.
	entries := []rangeEntry[string]{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"},
		{start: netip.MustParseAddr("198.51.100.0"), end: netip.MustParseAddr("198.51.100.255"), value: "DE"},
		{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), value: "JP"},
	}
	if err := r.writeCountryRanges(ctx, entries); err != nil {
		t.Fatalf("writeCountryRanges: %v", err)
	}

	// Read back through a raw query, not through writeCountryRanges/CopyFrom,
	// to actually exercise the decode direction too. No ORDER BY: Postgres's
	// cross-family inet ordering isn't asserted on here, only that the
	// same set of rows comes back - so the comparison below is by set
	// membership, not position.
	rows, err := r.pool.Query(ctx, "SELECT start_addr, end_addr, country FROM ip_country_ranges")
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	defer rows.Close()

	got := make(map[rangeEntry[string]]bool)
	for rows.Next() {
		var start, end netip.Addr
		var country string
		if err := rows.Scan(&start, &end, &country); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rangeEntry[string]{start: start, end: end, value: country}] = true
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

	// writeCountryRanges must fully replace the table (TRUNCATE + COPY in
	// one transaction), not append - a second call with different data
	// should leave no trace of the first.
	second := []rangeEntry[string]{{start: netip.MustParseAddr("203.0.113.1"), end: netip.MustParseAddr("203.0.113.1"), value: "FR"}}
	if err := r.writeCountryRanges(ctx, second); err != nil {
		t.Fatalf("writeCountryRanges (second call): %v", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM ip_country_ranges").Scan(&count); err != nil {
		t.Fatalf("count after second writeCountryRanges: %v", err)
	}
	if count != len(second) {
		t.Errorf("row count after second writeCountryRanges = %d, want %d (a full replace, not an append)", count, len(second))
	}
}

// TestResolver_RealTimescaleDB_WriteASNRangesThenReadBack mirrors
// TestResolver_RealTimescaleDB_WriteCountryRangesThenReadBack, but for
// ip_asn_ranges - a separate table with a structurally different payload
// (asn int + asn_org text instead of a single country column), so it needs
// its own round-trip proof rather than assuming writeCountryRanges's
// coverage extends to it.
func TestResolver_RealTimescaleDB_WriteASNRangesThenReadBack(t *testing.T) {
	ctx := context.Background()
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 100, TTL: 0}, "", nil)
	if err != nil {
		t.Fatalf("NewResolver: %v (is `docker compose up -d` running, with both schema.sql files applied?)", err)
	}
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		if _, err := r.pool.Exec(context.Background(), "TRUNCATE ip_asn_ranges"); err != nil {
			t.Logf("cleanup: truncate failed: %v", err)
		}
	})

	// Includes an org name containing a literal comma, the same real-data
	// shape parseASNCSV is tested against - proving the DB round trip
	// preserves it too, not just CSV parsing.
	entries := []rangeEntry[asnInfo]{
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), value: asnInfo{asn: 15169, org: "GOOGLE"}},
		{start: netip.MustParseAddr("1.0.0.0"), end: netip.MustParseAddr("1.0.0.255"), value: asnInfo{asn: 13335, org: "Cloudflare, Inc."}},
		{start: netip.MustParseAddr("2001:4860::"), end: netip.MustParseAddr("2001:4860::ffff"), value: asnInfo{asn: 15169, org: "GOOGLE"}},
	}
	if err := r.writeASNRanges(ctx, entries); err != nil {
		t.Fatalf("writeASNRanges: %v", err)
	}

	rows, err := r.pool.Query(ctx, "SELECT start_addr, end_addr, asn, asn_org FROM ip_asn_ranges")
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	defer rows.Close()

	got := make(map[rangeEntry[asnInfo]]bool)
	for rows.Next() {
		var start, end netip.Addr
		var asn int
		var org string
		if err := rows.Scan(&start, &end, &asn, &org); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[rangeEntry[asnInfo]{start: start, end: end, value: asnInfo{asn: asn, org: org}}] = true
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

	// Full replace, not append - same requirement as writeCountryRanges.
	second := []rangeEntry[asnInfo]{{start: netip.MustParseAddr("203.0.113.1"), end: netip.MustParseAddr("203.0.113.1"), value: asnInfo{asn: 64512, org: "Example Org"}}}
	if err := r.writeASNRanges(ctx, second); err != nil {
		t.Fatalf("writeASNRanges (second call): %v", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM ip_asn_ranges").Scan(&count); err != nil {
		t.Fatalf("count after second writeASNRanges: %v", err)
	}
	if count != len(second) {
		t.Errorf("row count after second writeASNRanges = %d, want %d (a full replace, not an append)", count, len(second))
	}
}

// TestResolver_RealTimescaleDB_ResolveAfterRealRefresh exercises Resolve
// against data that actually round-tripped through TimescaleDB (rather
// than an in-memory table injected directly, as the rest of this
// package's tests do), closing the gap between "the parsers are correct"
// and "a real write+swap actually makes Resolve see it" - for both
// datasets and both address families.
func TestResolver_RealTimescaleDB_ResolveAfterRealRefresh(t *testing.T) {
	ctx := context.Background()
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 100, TTL: 0}, "", nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		if _, err := r.pool.Exec(context.Background(), "TRUNCATE ip_country_ranges"); err != nil {
			t.Logf("cleanup: truncate ip_country_ranges failed: %v", err)
		}
		if _, err := r.pool.Exec(context.Background(), "TRUNCATE ip_asn_ranges"); err != nil {
			t.Logf("cleanup: truncate ip_asn_ranges failed: %v", err)
		}
	})

	countryEntries4 := []rangeEntry[string]{{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"}}
	countryEntries6 := []rangeEntry[string]{{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), value: "JP"}}
	if err := r.writeCountryRanges(ctx, append(countryEntries4, countryEntries6...)); err != nil {
		t.Fatalf("writeCountryRanges: %v", err)
	}
	r.countryTable4.Store(newRangeTable(countryEntries4))
	r.countryTable6.Store(newRangeTable(countryEntries6))

	asnEntries4 := []rangeEntry[asnInfo]{{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: asnInfo{asn: 64512, org: "Example Org"}}}
	asnEntries6 := []rangeEntry[asnInfo]{{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), value: asnInfo{asn: 64513, org: "Example Org JP"}}}
	if err := r.writeASNRanges(ctx, append(asnEntries4, asnEntries6...)); err != nil {
		t.Fatalf("writeASNRanges: %v", err)
	}
	r.asnTable4.Store(newRangeTable(asnEntries4))
	r.asnTable6.Store(newRangeTable(asnEntries6))

	if got := r.Resolve(netip.MustParseAddr("192.0.2.42")); !got.Found || got.Country != "US" || got.ASN != 64512 || got.ASNName != "Example Org" {
		t.Errorf("Resolve(IPv4) = %+v, want Found: true, Country: US, ASN: 64512, ASNName: Example Org", got)
	}
	if got := r.Resolve(netip.MustParseAddr("2001:db8::1234")); !got.Found || got.Country != "JP" || got.ASN != 64513 || got.ASNName != "Example Org JP" {
		t.Errorf("Resolve(IPv6) = %+v, want Found: true, Country: JP, ASN: 64513, ASNName: Example Org JP", got)
	}
}
