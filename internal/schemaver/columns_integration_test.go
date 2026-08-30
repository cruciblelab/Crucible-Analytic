//go:build integration

// The two directions of schema drift, against a real database.
//
// This is the same experiment that produced the measurement L1 was
// designed around, run again now that something is supposed to catch it.
// Before RequireColumns existed the result was:
//
//	NewWriter succeeded — Ping passed, startup silent
//	first write: column "asn_org" does not exist (SQLSTATE 42703)
//	written=0  failed=3  rows in the table: 0 of 3
//
// What has to be true now is that the same drift is refused at startup,
// and - just as important - that the *other* direction still works,
// because that one is a correct upgrade in progress.
package schemaver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func driftPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN; this test alters a table's shape")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestAMissingColumnIsRefused.
//
// The direction that loses rows. The assertion is not only that it
// fails, but that the message names the column - somebody is reading it
// while a service will not start.
func TestAMissingColumnIsRefused(t *testing.T) {
	pool := driftPool(t)
	ctx := context.Background()

	err := RequireColumns(ctx, pool, "traffic_snapshots",
		[]string{"time", "site_id", "asn_org", "sutun_yok_boyle_bir"})
	if err == nil {
		t.Fatal("a table missing a column this build writes was accepted")
	}
	if !strings.Contains(err.Error(), "sutun_yok_boyle_bir") {
		t.Errorf("the error does not name the missing column: %v", err)
	}
	// And not the ones that are there, or the message sends the reader
	// looking at columns that are fine.
	if strings.Contains(err.Error(), "site_id") {
		t.Errorf("the error names site_id, which is present: %v", err)
	}
}

// TestAnExtraColumnIsAccepted.
//
// The direction a correct upgrade passes through, and the one that must
// never become an error: the schema goes first, the binaries follow, and
// in between every running binary is looking at a table with columns it
// has never heard of.
//
// Measured before this check existed: a writer in that state writes
// perfectly (written=1, failed=0). Making it fatal would turn the safe
// half of an upgrade into an outage and force the order that loses data.
func TestAnExtraColumnIsAccepted(t *testing.T) {
	pool := driftPool(t)
	ctx := context.Background()

	// The real table has more columns than these; naming a subset is
	// exactly the "binary older than schema" case.
	if err := RequireColumns(ctx, pool, "traffic_snapshots",
		[]string{"time", "site_id"}); err != nil {
		t.Errorf("a table with columns this build does not name was refused: %v", err)
	}
}

// TestAnAbsentTableIsRefusedClearly.
//
// A deployment where the schema was never applied. It has to fail, and
// it has to say so in a way that does not read like a bug in the
// service.
func TestAnAbsentTableIsRefusedClearly(t *testing.T) {
	pool := driftPool(t)

	err := RequireColumns(context.Background(), pool, "boyle_bir_tablo_yok", []string{"time"})
	if err == nil {
		t.Fatal("a table that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "apply the schema") {
		t.Errorf("the error does not tell the reader what to do: %v", err)
	}
}
