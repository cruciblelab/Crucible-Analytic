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

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
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

// TestAWriterPointedAtTheWrongRoleIsToldWhichColumnsAreMissing.
//
// The mutation that got away, and the case that kills it.
//
// Swapping pg_catalog for information_schema passed every other test
// here, because they all run as a superuser and a superuser sees every
// column in either catalog. That is this project's oldest lesson
// repeating itself: a fixture more privileged than production does not
// test production.
//
// Measured, the two catalogs disagree exactly when the connected role
// holds no grant on the named table:
//
//	beacon_writer / traffic_snapshots -> information_schema 0, pg_catalog 14
//
// Which is what a config file with the wrong DSN produces. Through
// pg_catalog the answer is about columns; through information_schema it
// would be "this table does not exist" about a table that plainly does,
// sending whoever reads it to re-apply a schema that is already there.
func TestAWriterPointedAtTheWrongRoleIsToldWhichColumnsAreMissing(t *testing.T) {
	// beacon_writer, deliberately: it is a real role in every deployment
	// and it holds no grant at all on traffic_snapshots. Derived from
	// testdb rather than from an environment variable of its own - a
	// test that skips when nobody sets a variable is a test that is
	// green for the wrong reason.
	pool, err := pgxpool.New(context.Background(), testdb.DSN(testdb.Beacon))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// traffic_snapshots is there and has these columns; this role simply
	// cannot see them through information_schema.
	if err := RequireColumns(context.Background(), pool, "traffic_snapshots",
		[]string{"time", "site_id"}); err != nil {
		t.Errorf("the columns were not found through a role with no grant on the table: %v\n"+
			"That is what information_schema answers here, and it is the wrong sentence: "+
			"the table exists, the role cannot see it", err)
	}
}
