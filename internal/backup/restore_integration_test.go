//go:build integration

// The restore, into a database that is really wiped and really rebuilt.
//
// Every check in this file is one that cannot be made without a second
// database: whether the wipe refuses the live one, whether the schema
// comes back, whether the rows arrive. The unit tests elsewhere in this
// package prove the file format; this proves the thing the file is for.
//
// *Hiçbir zaman denenmemiş bir yedek gönderilmiyor.*

package backup_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// quiet is a logger that writes nowhere.
//
// The restore logs a warning before it wipes anything, which is right in
// production and noise in a suite that wipes a scratch database forty
// times.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// schemaForRestore is internal/schemafiles in the shape the restore
// takes, the same conversion cmd/upgrader does.
func schemaForRestore() []backup.SchemaFile {
	out := make([]backup.SchemaFile, 0, len(schemafiles.InOrder))
	for _, f := range schemafiles.InOrder {
		out = append(out, backup.SchemaFile{Path: f.Path, SQL: f.SQL})
	}
	return out
}

// sideDatabase creates a scratch database owned by the connecting role
// and returns a pool on it.
//
// Created and dropped by the test rather than by the product, which is
// the arrangement the product ships: schema_admin holds no CREATEDB, and
// an operator makes this database once with a single statement. The
// suite stands in for that operator.
func sideDatabase(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	admin := testdb.Admin(t)
	ctx := context.Background()

	drop := func() {
		// Terminated first: a pool left open would make DROP DATABASE
		// wait for it, and a cleanup that hangs is worse than one that
		// fails.
		if _, err := admin.Exec(ctx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`,
			name); err != nil {
			t.Logf("terminating connections to %s: %v", name, err)
		}
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	}
	drop()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(drop)

	dsn := os.Getenv("CA_SUPERUSER_DSN")
	target, err := pgxpool.New(ctx, replaceDatabase(dsn, name))
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(target.Close)
	return target
}

// replaceDatabase swaps the database name in a DSN.
func replaceDatabase(dsn, name string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	rest := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		rest = dsn[slash+q:]
	}
	return dsn[:slash+1] + name + rest
}

// takeBackup queues one and runs it, returning the catalogue row.
func takeBackup(t *testing.T, asks, answers *pgxpool.Pool, dir string) backup.Backup {
	t.Helper()
	ctx := context.Background()
	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: schemaver.Version}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("taking the backup this test restores: %v", err)
	}
	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the catalogue has %d rows after one backup", len(rows))
	}
	return rows[0]
}

// TestARealBackupRestoresIntoARealDatabase.
//
// The done-criterion PLAN.md set for this whole group: "F1b'nin bitmiş
// sayılma şartı gerçek bir geri yükleme: ayrı bir veritabanına, satır
// satır karşılaştırmalı."
func TestARealBackupRestoresIntoARealDatabase(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	target := sideDatabase(t, "ca_restore_test")

	// Rows this test wrote, under a label no other suite uses.
	//
	// # Why not "the live table still holds at least what came back"
	//
	// Because that was the first version of this check and it was a
	// race, which CI found twice after passing here three times:
	//
	//	panel_users: 1 rows restored from a table that holds 0.
	//	The backup cannot contain more than was there
	//
	// The backup was correct. `go test ./...` runs packages in parallel
	// against one database, and another suite deleted its fixture user
	// between the dump and the count - so the live side shrank while the
	// restored side, taken from a file, did not.
	//
	// roundtrip_integration_test.go had already met this exact failure
	// and written down the answer; this file was the same mistake made
	// again three weeks later. A count nothing else writes is the only
	// count two goroutines cannot disagree about.
	const marker = "geri-yukleme-isareti"
	const seeded = 7
	if _, err := answers.Exec(ctx, `
		INSERT INTO panel_audit_log (actor_kind, actor_label, action, target)
		SELECT 'system', $1, 'yedek_testi', 'satir-' || g
		FROM generate_series(1, $2) AS g`, marker, seeded); err != nil {
		t.Fatalf("seeding the rows this test counts: %v", err)
	}
	t.Cleanup(func() {
		if _, err := answers.Exec(context.Background(),
			`DELETE FROM panel_audit_log WHERE actor_label = $1`, marker); err != nil {
			t.Errorf("clearing the seeded rows: %v", err)
		}
	})

	row := takeBackup(t, asks, answers, t.TempDir())

	report, err := backup.RestoreInto(ctx, answers, target, row.Path,
		schemaForRestore(), quiet())
	if err != nil {
		t.Fatalf("a backup this program just wrote did not restore: %v", err)
	}
	if report.Database != "ca_restore_test" {
		t.Errorf("the report names %q", report.Database)
	}
	if len(report.Tables) == 0 {
		t.Fatal("nothing was restored, and it reported success")
	}

	// What the report claims arrived, against what the target actually
	// holds. Two different places - a struct the restore built and a
	// database somebody else can count - so this one cannot race and
	// cannot agree with itself.
	for _, tbl := range report.Tables {
		var restored int64
		if err := target.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %q`, tbl.Table)).Scan(&restored); err != nil {
			t.Fatalf("counting %s in the restored database: %v", tbl.Table, err)
		}
		if restored != tbl.Rows {
			t.Errorf("%s: the report says %d rows arrived and the database holds %d",
				tbl.Table, tbl.Rows, restored)
		}
	}

	// And the seven rows this test wrote, which say the numbers above
	// are of real rows rather than a consistent count of nothing.
	var mine int64
	if err := target.QueryRow(ctx,
		`SELECT count(*) FROM panel_audit_log WHERE actor_label = $1`,
		marker).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if mine != seeded {
		t.Errorf("%d rows were written before the backup and %d came out of it.\n"+
			"A restore that agreed with its own manifest about zero rows would "+
			"pass every check above", seeded, mine)
	}

	// And the marker, which is what makes a second restore possible.
	var markers int
	if err := target.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`,
		backup.RestoreMarker).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 1 {
		t.Errorf("the target carries %d markers; without one the next restore refuses it",
			markers)
	}

	// A second restore into the same database works, because the marker
	// says this feature owns it.
	if _, err := backup.RestoreInto(ctx, answers, target, row.Path,
		schemaForRestore(), quiet()); err != nil {
		t.Fatalf("a second restore into a database this feature already owns: %v", err)
	}
}

// The refusal that matters more than the feature.
//
// Pointing restore_dsn at the live database would destroy everything
// this product holds, and it is a one-character mistake in a config
// file. Two things refuse it and this checks the one that produces the
// sentence.
func TestRestoringIntoTheLiveDatabaseIsRefused(t *testing.T) {
	_, answers := backupQueue(t)
	ctx := context.Background()

	_, err := backup.RestoreInto(ctx, answers, answers, "yok.tar.gz",
		schemaForRestore(), quiet())
	if err == nil {
		t.Fatal("a restore into the live database was allowed")
	}
	if !errors.Is(err, backup.ErrLiveDatabase) {
		t.Fatalf("got %v, want ErrLiveDatabase", err)
	}

	// And nothing was touched: the live database still has its tables.
	var tables int
	if err := answers.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname='public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables < 10 {
		t.Fatalf("the live database has %d tables left. This is the failure this whole "+
			"file exists to make impossible", tables)
	}
}

// And the check that is the enforcement: a database with somebody
// else's tables in it.
//
// Separate from the one above because it catches more - the live
// database is one case of "a database this feature does not own", and
// the others are every other real database on the machine.
func TestRestoringIntoADatabaseWithStrangersTablesIsRefused(t *testing.T) {
	_, answers := backupQueue(t)
	ctx := context.Background()
	target := sideDatabase(t, "ca_restore_notmine_test")

	if _, err := target.Exec(ctx,
		`CREATE TABLE baskasinin_tablosu (id int)`); err != nil {
		t.Fatal(err)
	}

	_, err := backup.RestoreInto(ctx, answers, target, "yok.tar.gz",
		schemaForRestore(), quiet())
	if !errors.Is(err, backup.ErrNotARestoreTarget) {
		t.Fatalf("got %v, want ErrNotARestoreTarget", err)
	}

	// Untouched.
	var there int
	if err := target.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename=$1`,
		"baskasinin_tablosu").Scan(&there); err != nil {
		t.Fatal(err)
	}
	if there != 1 {
		t.Error("a refused restore destroyed the table it refused to write over")
	}
}

// The whole chain, from the row somebody writes to the report on it.
func TestPressingRestoreProducesAReportOnTheRequest(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	target := sideDatabase(t, "ca_restore_queue_test")

	row := takeBackup(t, asks, answers, t.TempDir())

	if _, err := backup.AskRestore(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		row.ID); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: t.TempDir(), Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: schemaver.Version,
		RestorePool: target, Schema: schemaForRestore()}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("the restore failed: %v", err)
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateSucceeded {
		t.Fatalf("the request is in state %q: %s", latest.State, latest.ErrorChain)
	}
	if latest.Work != backup.WorkRestore {
		t.Errorf("the row says work=%q", latest.Work)
	}
	// The report is the point. A restore that succeeded and said nothing
	// would leave somebody rehearsing a recovery with no numbers.
	if latest.Result == "" {
		t.Fatal("the request carries no result, so the page has nothing to show for it")
	}
	for _, want := range []string{"ca_restore_queue_test", "satır", "süre"} {
		if !strings.Contains(latest.Result, want) {
			t.Errorf("the result does not mention %q:\n%s", want, latest.Result)
		}
	}
}

// A deployment with no side database says so on the row, rather than
// waiting for an upgrader that can never serve it.
func TestWithoutASideDatabaseTheRequestFailsWithTheReason(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	if _, err := backup.AskRestore(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		1); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: t.TempDir(), Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: schemaver.Version,
		Schema: schemaForRestore()}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("a restore ran with nowhere to restore into")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateFailed {
		t.Fatalf("the request is in state %q, want failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "restore_dsn") {
		t.Errorf("the row says %q and does not say what to do about it", latest.ErrorChain)
	}
}

// A secrets backup is not rows and there is nowhere to put it.
//
// # Why this is a sentence and not a crash
//
// The two kinds of backup sit in one list with the same buttons beside
// them, so pressing restore on the wrong row is a reasonable mistake
// rather than a strange one. Without this the attempt would wipe the
// side database, rebuild it, and then fail looking for a manifest that
// is not the shape it expects - destroying something for nothing and
// reporting a confusing reason.
//
// So it is refused before the wipe, and the refusal says what to do
// instead.
func TestRestoringASecretsBackupIsRefusedWithSomewhereElseToGo(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	target := sideDatabase(t, "ca_restore_secrets_test")
	conf := secretsConfDir(t)

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetSirlar}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: t.TempDir(), ConfDir: conf,
		Recipient: secretsRecipient(t), Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: schemaver.Version,
		RestorePool: target, Schema: schemaForRestore()}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("taking the secrets backup this test points at: %v", err)
	}
	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}

	// The side database as it is before anything is asked of it, so the
	// assertion below is about this restore rather than about history.
	before := tableCount(t, target)

	if _, err := backup.AskRestore(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("a secrets backup was restored into a database as if it were rows")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateFailed {
		t.Fatalf("the request is in state %q, want failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "devpass -open") {
		t.Errorf("the row says %q and does not say where to go instead", latest.ErrorChain)
	}

	// And the side database was not wiped on the way to that refusal.
	if after := tableCount(t, target); after != before {
		t.Errorf("the side database had %d tables and now has %d: it was destroyed by a "+
			"request that was going to be refused anyway", before, after)
	}
}

// tableCount is how many tables the public schema holds.
func tableCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A restore with no schema to build is refused before anything is
// touched.
//
// Reachable only from a build whose wiring is wrong - cmd/upgrader
// supplies the list and TestTheRestoreSchemaIsTheWholeSchema checks it
// - and refused anyway, because the alternative is worse than an
// error. Without this the target is wiped, rebuilt as an empty
// database, and the first COPY fails looking for a table that was never
// created. The message would send somebody to look at their backup
// file, and their backup file is fine.
//
// Checked before the database is even named, so a build with this
// defect destroys nothing while it has it.
func TestARestoreWithNoSchemaIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	_, answers := backupQueue(t)

	_, err := backup.RestoreInto(context.Background(), answers, answers, "yok.tar.gz",
		nil, quiet())
	if err == nil {
		t.Fatal("a restore ran with no schema to build the database from")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
	// Refused before the live-database check, which is the only way to
	// tell that nothing about the target was consulted.
	if errors.Is(err, backup.ErrLiveDatabase) {
		t.Error("the schema check runs after the target is inspected; it has to be first, " +
			"or a build with this defect gets as far as looking at a database")
	}
}
