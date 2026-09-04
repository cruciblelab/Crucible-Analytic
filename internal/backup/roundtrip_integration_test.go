//go:build integration

// The only test that says this feature works.
//
// # Why a restore, and not a file that looks right
//
// The design here was chosen against a measured failure, not a guessed
// one. `pg_dump --table=traffic_snapshots` produces a file. The file is
// valid, it is a plausible size, it restores without a warning, and it
// contains zero rows - because a hypertable's rows are in chunks and the
// --table filter does not follow them.
//
// Every check short of a restore passes against that file. It exists, it
// has bytes, its checksum is stable, it decompresses, its manifest
// parses. The only thing that catches it is putting the rows back and
// counting them.
//
// So that is what this does, into a database it creates for the purpose,
// with the schema built from the same embedded files an install uses.
//
// *Bir yedeğin var olması, geri yüklenebileceği anlamına gelmez.*

package backup_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// scratchDatabase makes an empty database with this product's schema in
// it and returns a pool for it.
//
// Built from schemafiles rather than from the backup, which is the
// design being tested: the tables come from the binary and the rows come
// from the file. A test that let the backup create its own tables would
// prove a different product works.
func scratchDatabase(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	admin := testdb.Admin(t)
	ctx := context.Background()

	for _, sql := range []string{
		`DROP DATABASE IF EXISTS ` + name,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})

	// The same superuser connection, pointed at the new database. Built
	// by swapping the database in the DSN rather than by asking for a
	// second environment variable: an operator who set one and not the
	// other would get a test that restored into the live database.
	pool, err := pgxpool.New(ctx, swapDatabase(os.Getenv("CA_SUPERUSER_DSN"), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		t.Fatalf("timescaledb in %s: %v", name, err)
	}
	for _, f := range schemafiles.InOrder {
		if _, err := pool.Exec(ctx, f.SQL); err != nil {
			// Grants name roles that exist cluster-wide, so this should
			// apply cleanly; if it does not, the message names the file.
			t.Fatalf("applying %s to %s: %v", f.Path, name, err)
		}
	}
	return pool
}

// TestABackupRestoresEveryRowItSaysItHas.
func TestABackupRestoresEveryRowItSaysItHas(t *testing.T) {
	ctx := context.Background()
	source := testdb.Pool(t, testdb.SchemaAdmin)

	// Rows this test put there, so the counts are its own rather than
	// whatever the shared database happens to hold.
	const site = "yedek-turu"
	writer := testdb.Pool(t, testdb.Collector)
	if _, err := writer.Exec(ctx, `
		INSERT INTO traffic_snapshots
		    (time, site_id, ip, ja4, prev_window_count, curr_window_count,
		     request_rate, bot_score, is_known_bot_ja4,
		     country, asn, asn_org, is_known_bot_asn)
		SELECT now() - (g || ' seconds')::interval, $1,
		       ('203.0.113.' || (g % 256))::inet,
		       't13d1516h2_8daaf6152771_b186095e22b6',
		       0, 0, 0, 0, false, 'TR', 0, '', false
		FROM generate_series(1, 500) AS g`, site); err != nil {
		t.Fatalf("seeding traffic: %v", err)
	}
	t.Cleanup(func() {
		if _, err := source.Exec(context.Background(),
			`DELETE FROM traffic_snapshots WHERE site_id = $1`, site); err != nil {
			t.Errorf("clearing the seeded rows: %v", err)
		}
	})

	// The seeded rows are counted, and nothing else is.
	//
	// # Why not "the table had N rows before and N after"
	//
	// Because that is a race, and it lost one: `go test ./...` runs
	// packages in parallel against one database, and beacon_events went
	// from 11676 rows before the dump to 11721 by the time the restore
	// was counted. Another suite was writing. The test failed on a
	// backup that was entirely correct.
	//
	// Two comparisons that cannot race replace it. The manifest's count
	// against what the restore put back, which says the file is
	// internally whole; and this test's own five hundred rows, which no
	// other suite writes, which says the rows are real ones rather than
	// a consistent count of nothing.
	dir := t.TempDir()
	w := backup.Writer{Pool: source, Dir: dir, BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	res, err := w.Write(ctx, "test.tar.gz", []string{backup.SetAnalitik, backup.SetPanel})
	if err != nil {
		t.Fatal(err)
	}

	// The file exists, is not empty, and is not readable by anybody else.
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("the backup file is empty")
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the backup is mode %v. It holds every row the four services are not "+
			"allowed to read, plus the panel's password hashes and TOTP secrets", perm)
	}

	// And now the part nothing else proves.
	target := scratchDatabase(t, "ca_backup_roundtrip")
	restored, err := backup.Restore(ctx, target, res.Path)
	if err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if len(restored) == 0 {
		t.Fatal("the restore put nothing back, and reported success")
	}

	for _, r := range restored {
		got := countRows(t, target, r.Table)
		if got != r.Wanted {
			t.Errorf("%s: the manifest says %d rows and %d are in the restored "+
				"database.\n"+
				"This is the check that catches a dump which produced a valid, "+
				"plausible, empty file - which is what pg_dump's --table filter does "+
				"to a hypertable", r.Table, r.Wanted, got)
		}
	}

	// The rows this test wrote, which nothing else touches.
	var seeded int64
	if err := target.QueryRow(ctx,
		`SELECT count(*) FROM traffic_snapshots WHERE site_id = $1`, site).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if seeded != 500 {
		t.Errorf("five hundred rows were written before the backup and %d came out of "+
			"it. A count that matches a manifest of zero would still be consistent; "+
			"these are rows whose number this test chose", seeded)
	}

	// Every table the sets name reached the file. A set that resolved to
	// nothing would restore cleanly and prove nothing.
	seen := map[string]bool{}
	for _, r := range restored {
		seen[r.Table] = true
	}
	for _, table := range backup.SortedTableNames() {
		if !seen[table] {
			t.Errorf("%s is in a set and no data for it was in the backup", table)
		}
	}
}

// TestTheHypertablesAreTheOnesThatWouldHaveBeenEmpty.
//
// The narrow claim, stated separately because it is the whole reason
// this package copies rather than dumps. If these two ever come back
// empty while the ordinary tables are fine, the cause is the filter
// problem returning by another route.
func TestTheHypertablesAreTheOnesThatWouldHaveBeenEmpty(t *testing.T) {
	ctx := context.Background()
	source := testdb.Pool(t, testdb.SchemaAdmin)

	for _, table := range []string{"traffic_snapshots", "beacon_events"} {
		if countRows(t, source, table) == 0 {
			t.Skipf("%s is empty in this database, so an empty copy would prove nothing",
				table)
		}
	}

	dir := t.TempDir()
	w := backup.Writer{Pool: source, Dir: dir, BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	res, err := w.Write(ctx, "hyper.tar.gz", []string{backup.SetAnalitik})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range res.Manifest.Tables {
		if entry.Rows == 0 {
			t.Errorf("%s went into the backup with zero rows while the table is not "+
				"empty. That is the pg_dump failure this package exists to avoid, and "+
				"the file it produces looks entirely normal", entry.Name)
		}
		if entry.Bytes == 0 {
			t.Errorf("%s contributed no bytes to the backup", entry.Name)
		}
	}
}

// TestAnInterruptedBackupNeverTakesTheFinalName.
//
// A file killed half way is a valid gzip stream of the right shape,
// missing its last table and its manifest. Under the final name it would
// sit in the catalogue as a backup somebody could rely on.
func TestAnInterruptedBackupNeverTakesTheFinalName(t *testing.T) {
	source := testdb.Pool(t, testdb.SchemaAdmin)
	dir := t.TempDir()

	// A context that ends while the copy is running.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	w := backup.Writer{Pool: source, Dir: dir}
	if _, err := w.Write(ctx, "yarim.tar.gz", []string{backup.SetAnalitik, backup.SetPanel}); err == nil {
		t.Skip("the copy finished inside the deadline; nothing was interrupted")
	}

	if _, err := os.Stat(filepath.Join(dir, "yarim.tar.gz")); err == nil {
		t.Error("an interrupted backup is sitting under its final name. Nothing about " +
			"such a file looks wrong: it decompresses, it has a size, and it is missing " +
			"the tables that had not been copied yet")
	}
	// And the temporary file is gone too, so a retry does not accumulate.
	left, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range left {
		t.Errorf("%s was left behind by a failed backup", filepath.Base(p))
	}
}

// TestASetThisBuildDoesNotKnowIsRefused.
//
// A request written by an older panel, naming a set that has since been
// renamed. Ignoring it would produce a backup missing what somebody
// asked for, reported as a success.
func TestASetThisBuildDoesNotKnowIsRefused(t *testing.T) {
	_, err := backup.TablesFor([]string{backup.SetPanel, "bilinmeyen-kume"})
	if err == nil {
		t.Fatal("a set name this build does not know was accepted")
	}
	if !strings.Contains(err.Error(), "bilinmeyen-kume") {
		t.Errorf("the refusal does not name the set: %v", err)
	}
}

// swapDatabase points a DSN at a different database.
func swapDatabase(dsn, name string) string {
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		return dsn[:i+1] + name + rest[q:]
	}
	return dsn[:i+1] + name
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %q`, table)).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}
