//go:build integration

package backup_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
)

// The age limit, against real files and a real catalogue.
//
// # What this is about
//
// The retention policy deletes analytics rows past their age, and a
// backup taken before that day still holds them. So the backup
// directory is the one place the retention number quietly does not
// apply - which makes it the one place a deployment can keep a
// customer's visitors' data for as long as its disk lasts, while every
// page in the product says otherwise.
//
// F1's own prose said this from the beginning and the phase table never
// carried a line for it, so eight sub-phases finished and the ninth was
// never counted.
//
// # Why every case is a file on disk
//
// Because the operation is two halves that can disagree: the file goes,
// then the row. A test that only counted rows would pass on an
// implementation that forgot the row and left the file - which is a
// backup nobody will verify, nobody will list, and nobody will delete.
// So each case asserts on both.

// aged records a catalogue row for a real file with a chosen age.
//
// The file is written rather than faked, because half of what Expire
// does is os.Remove and a test whose files do not exist cannot tell a
// deletion from a no-op.
func aged(t *testing.T, answers *pgxpool.Pool, dir, name string,
	sets []string, age time.Duration) (int64, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("yedek-"+name), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := backup.Record(context.Background(), answers, backup.Result{
		Path:          path,
		Bytes:         int64(len("yedek-" + name)),
		SHA256:        fmt.Sprintf("%064x", len(name)),
		TakenAt:       time.Now().Add(-age),
		Sets:          sets,
		BinaryVersion: "v0.0.0-test",
		SchemaVersion: 99,
	})
	if err != nil {
		t.Fatalf("recording %s: %v", name, err)
	}
	return id, path
}

// present reports whether the catalogue still names this row.
func present(t *testing.T, answers *pgxpool.Pool, id int64) bool {
	t.Helper()
	var n int
	if err := answers.QueryRow(context.Background(),
		`SELECT count(*) FROM panel_backups WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func onDisk(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestTheAgeLimitDeletesTheOldOnesAndKeepsTheLast.
//
// Four rows in one run rather than four tests, because what is being
// asserted is a *choice among them*: the same sweep has to delete one,
// keep another for being young, keep a third for being the newest, and
// keep a fourth for being secrets. Four separate tests would each set up
// a directory where their own row was the only candidate, which is the
// one arrangement in which no choosing happens.
func TestTheAgeLimitDeletesTheOldOnesAndKeepsTheLast(t *testing.T) {
	_, answers := backupQueue(t)
	dir := t.TempDir()

	data := []string{backup.SetAnalitik}
	secrets := []string{backup.SetSirlar}

	oldID, oldPath := aged(t, answers, dir, "eski.tar.gz", data, 40*24*time.Hour)
	youngID, youngPath := aged(t, answers, dir, "yeni.tar.gz", data, 3*24*time.Hour)
	// Older than the limit and still the newest *data* backup is
	// impossible in one run, so the newest-is-kept case gets its own
	// run below. Here the newest is simply the young one.
	secretsID, secretsPath := aged(t, answers, dir, "sirlar-eski.tar.gz", secrets, 90*24*time.Hour)

	r := backup.Runner{Pool: answers, Dir: dir, KeepDays: 30}
	gone, err := r.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if gone != 1 {
		t.Errorf("Expire deleted %d backups, want 1", gone)
	}

	for _, c := range []struct {
		what string
		id   int64
		path string
		want bool
	}{
		{"the 40-day-old data backup", oldID, oldPath, false},
		{"the 3-day-old data backup", youngID, youngPath, true},
		{"the 90-day-old secrets backup", secretsID, secretsPath, true},
	} {
		if got := present(t, answers, c.id); got != c.want {
			t.Errorf("%s: catalogue row present = %v, want %v", c.what, got, c.want)
		}
		if got := onDisk(t, c.path); got != c.want {
			t.Errorf("%s: file on disk = %v, want %v.\n"+
				"The file and the row are two halves and either one left behind is a "+
				"defect: a file with no row is never verified and never deleted, and a "+
				"row with no file is a page offering a backup that is not there",
				c.what, got, c.want)
		}
	}
}

// TestTheNewestBackupSurvivesTheAgeLimit.
//
// # Why this is a guard and not a setting
//
// A deployment whose last backup was taken four hundred days ago, under
// a three-hundred-day limit, would otherwise be swept to *no backup at
// all*. "An old backup" and "no backup" are not two points on one
// scale: the first restores a customer to last year, the second
// restores nothing.
//
// The limit exists so data does not outlive its retention. The last
// file is where that reason stops paying for itself.
func TestTheNewestBackupSurvivesTheAgeLimit(t *testing.T) {
	_, answers := backupQueue(t)
	dir := t.TempDir()
	data := []string{backup.SetAnalitik}

	// Both far past the limit, so nothing here is kept for being young.
	olderID, olderPath := aged(t, answers, dir, "cok-eski.tar.gz", data, 400*24*time.Hour)
	newestID, newestPath := aged(t, answers, dir, "eski.tar.gz", data, 350*24*time.Hour)

	r := backup.Runner{Pool: answers, Dir: dir, KeepDays: 300}
	gone, err := r.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if gone != 1 {
		t.Errorf("Expire deleted %d backups, want 1 - the older of the two", gone)
	}
	if present(t, answers, olderID) || onDisk(t, olderPath) {
		t.Error("the 400-day-old backup outlived a 300-day limit")
	}
	if !present(t, answers, newestID) || !onDisk(t, newestPath) {
		t.Error("the newest backup was deleted for being older than the limit.\n" +
			"That leaves the deployment with no backup at all, which is the one " +
			"outcome the limit was never meant to produce")
	}
}

// TestNoLimitDeletesNothing.
//
// The default, and the state every deployment upgrades into. A version
// that started pruning on the day it was installed would delete a
// customer's backups as a side effect of an upgrade they asked for for
// another reason.
// # Two backups, and the second one is the whole test
//
// The first version recorded one ten-year-old backup and asserted that
// nothing went. A mutation turning `KeepDays <= 0` into `KeepDays < 0`
// walked straight through it: with the limit read as zero the cutoff
// becomes "now", every backup is older than that - and the only row in
// the directory was also the newest, so the guard that keeps the last
// one saved it and the test went green.
//
// So the arrangement had to stop being the one arrangement where the
// mutation cannot show. Two rows: an old one that nothing protects, and
// a newer one so the old one is not the last.
//
// *Bir korumanın arkasına saklanan bir test, koruduğu şeyi sınamaz.*
func TestNoLimitDeletesNothing(t *testing.T) {
	_, answers := backupQueue(t)
	dir := t.TempDir()
	data := []string{backup.SetAnalitik}

	oldID, oldPath := aged(t, answers, dir, "cok-eski.tar.gz", data, 3650*24*time.Hour)
	newID, newPath := aged(t, answers, dir, "yeni.tar.gz", data, time.Hour)

	r := backup.Runner{Pool: answers, Dir: dir, KeepDays: 0}
	gone, err := r.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire with keep_days=0: %v", err)
	}
	if gone != 0 {
		t.Errorf("keep_days=0 deleted %d backups", gone)
	}
	for _, c := range []struct {
		what string
		id   int64
		path string
	}{
		{"the ten-year-old backup", oldID, oldPath},
		{"the one-hour-old backup", newID, newPath},
	} {
		if !present(t, answers, c.id) || !onDisk(t, c.path) {
			t.Errorf("%s was deleted by a deployment that set no limit", c.what)
		}
	}
}

// TestAMissingFileIsStillForgottenWhenItIsOldEnough.
//
// The row an operator's own `rm` left behind, marked missing by Sweep.
// It has no file to delete and it is still past the age this deployment
// keeps backups for, so the sentence it was preserving - "there was a
// backup here" - is about a file that would have gone anyway.
//
// Asserted because the alternative is a catalogue that grows forever on
// exactly the deployments that prune, which is the shape of bug that
// only shows up after a year.
func TestAMissingFileIsStillForgottenWhenItIsOldEnough(t *testing.T) {
	_, answers := backupQueue(t)
	dir := t.TempDir()
	data := []string{backup.SetAnalitik}

	// A newer one, so the row under test is not the newest and is not
	// kept by that guard.
	if _, err := answers.Exec(context.Background(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	_, _ = aged(t, answers, dir, "yeni.tar.gz", data, time.Hour)

	goneID, gonePath := aged(t, answers, dir, "silinmis.tar.gz", data, 40*24*time.Hour)
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: dir, KeepDays: 30}
	if marked, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	} else if marked != 1 {
		t.Fatalf("Sweep marked %d rows missing, want 1", marked)
	}

	if _, err := r.Expire(context.Background()); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if present(t, answers, goneID) {
		t.Error("a row marked missing and past the age limit was kept")
	}
}
