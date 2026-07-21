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
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 100, TTL: 0}, nil)
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

	entries := []rangeEntry{
		{start: 0xC0000200, end: 0xC00002FF, country: "US"}, // 192.0.2.0/24
		{start: 0xC6336400, end: 0xC63367FF, country: "DE"}, // 198.51.100.0/22
	}
	if err := r.writeRanges(ctx, entries); err != nil {
		t.Fatalf("writeRanges: %v", err)
	}

	// Read back through a raw query, not through writeRanges/CopyFrom, to
	// actually exercise the decode direction too.
	rows, err := r.pool.Query(ctx, "SELECT start_addr, end_addr, country FROM ip_country_ranges ORDER BY start_addr")
	if err != nil {
		t.Fatalf("query back: %v", err)
	}
	defer rows.Close()

	var got []rangeEntry
	for rows.Next() {
		var start, end int64
		var country string
		if err := rows.Scan(&start, &end, &country); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, rangeEntry{start: uint32(start), end: uint32(end), country: country})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != len(entries) {
		t.Fatalf("read back %d rows, want %d: %+v", len(got), len(entries), got)
	}
	for i, want := range entries {
		if got[i] != want {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want)
		}
	}

	// writeRanges must fully replace the table (TRUNCATE + COPY in one
	// transaction), not append - a second call with different data should
	// leave no trace of the first.
	second := []rangeEntry{{start: 1, end: 2, country: "FR"}}
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
// and "a real write+swap actually makes Resolve see it."
func TestResolver_RealTimescaleDB_ResolveAfterRealRefresh(t *testing.T) {
	ctx := context.Background()
	r, err := NewResolver(ctx, testDatabaseURL, CacheConfig{MaxEntries: 100, TTL: 0}, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		if _, err := r.pool.Exec(context.Background(), "TRUNCATE ip_country_ranges"); err != nil {
			t.Logf("cleanup: truncate failed: %v", err)
		}
	})

	entries := []rangeEntry{{start: 0xC0000200, end: 0xC00002FF, country: "US"}}
	if err := r.writeRanges(ctx, entries); err != nil {
		t.Fatalf("writeRanges: %v", err)
	}
	r.table.Store(newRangeTable(entries))

	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	if !got.Found || got.Country != "US" {
		t.Errorf("Resolve() = %+v, want Found: true, Country: US", got)
	}
}
