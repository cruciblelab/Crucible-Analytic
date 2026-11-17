//go:build integration

package preflight

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The wizard's backup check, against the backup tables a deployment has.
//
// It said "Bu sistem yedek almaz ve alınıp alınmadığını kontrol edemez"
// on every deployment, including ones with a backup taken the day before:
// the check predated the backup feature and never asked it anything (W4,
// item 2). Each case below is a state the catalogue, the take queue and
// the upgrader's policy row can be in, and the sentence the check owes it.
//
// In a schema of its own, with copies of the three tables derived by
// LIKE: internal/backup's suites write the shared ones while this runs,
// and a catalogue row of theirs would make every case here a question
// about scheduling. panel_user is granted the copy the way grants.sql
// grants the original - every catalogue column but `path` - so a check
// that started reading where the files are would be refused here too.

const backupCheckSchema = "ca_preflight_backup"

type backupFixture struct {
	t     *testing.T
	admin *pgxpool.Pool
	c     *Checker
	now   time.Time
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	ctx := context.Background()
	admin := testdb.Admin(t)
	s := backupCheckSchema
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS ` + s + ` CASCADE`,
		`CREATE SCHEMA ` + s,
		`CREATE TABLE ` + s + `.panel_backups (LIKE public.panel_backups INCLUDING ALL)`,
		`CREATE TABLE ` + s + `.panel_backup_requests (LIKE public.panel_backup_requests INCLUDING ALL)`,
		`CREATE TABLE ` + s + `.panel_backup_policy (LIKE public.panel_backup_policy INCLUDING ALL)`,
		`GRANT USAGE ON SCHEMA ` + s + ` TO panel_user`,
		`GRANT SELECT (id, taken_at, sets, bytes, sha256, binary_version, schema_version, state,
			device, verified_at, verify_problems) ON ` + s + `.panel_backups TO panel_user`,
		`GRANT SELECT ON ` + s + `.panel_backup_requests TO panel_user`,
		`GRANT SELECT ON ` + s + `.panel_backup_policy TO panel_user`,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+s+` CASCADE`); err != nil {
			t.Errorf("dropping %s: %v", s, err)
		}
	})

	pool, err := pgxpool.New(ctx, testDatabaseURL+"?options=-csearch_path%3D"+s+",public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, table := range []string{"panel_backups", "panel_backup_requests", "panel_backup_policy"} {
		var where string
		if err := pool.QueryRow(ctx, `SELECT n.nspname FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = $1::regclass`, table).
			Scan(&where); err != nil {
			t.Fatal(err)
		}
		if where != s {
			t.Fatalf("this pool resolves %s to %q, not to the copy in %q", table, where, s)
		}
	}
	return &backupFixture{t: t, admin: admin, c: New(pool), now: time.Now()}
}

func (f *backupFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.admin.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *backupFixture) policy() {
	f.exec(`INSERT INTO ` + backupCheckSchema + `.panel_backup_policy (id, keep_days) VALUES (1, 0)`)
}

// take records a finished request to take a backup, failed when cause is
// not empty, requested ago before now.
func (f *backupFixture) take(ago time.Duration, cause string) {
	state := backup.StateSucceeded
	if cause != "" {
		state = backup.StateFailed
	}
	f.exec(`INSERT INTO `+backupCheckSchema+`.panel_backup_requests
		(requested_at, actor_kind, kind, sets, state, finished_at, error_chain)
		VALUES ($1, 'user', 'al', '{panel}', $2, $1, $3)`,
		f.now.Add(-ago), string(state), cause)
}

// backup records a catalogue row taken ago before now.
func (f *backupFixture) backup(ago time.Duration, state string, verified *time.Time, problems string) {
	f.exec(`INSERT INTO `+backupCheckSchema+`.panel_backups
		(taken_at, sets, bytes, sha256, path, state, verified_at, verify_problems)
		VALUES ($1, '{panel}', 734003200, 'abc', '/var/lib/x', $2, $3, $4)`,
		f.now.Add(-ago), state, verified, problems)
}

func (f *backupFixture) check() CheckResult {
	f.t.Helper()
	return find(f.t, f.c.Run(context.Background(), Config{Now: func() time.Time { return f.now }}),
		"backup.configured")
}

// The old sentence is gone from everything the wizard prints, whichever
// state the deployment is in.
func assertNotTheOldSentence(t *testing.T, r CheckResult) {
	t.Helper()
	for _, text := range []string{r.Detail, r.Fix} {
		if strings.Contains(text, "yedek almaz") {
			t.Errorf("the check still says the system takes no backups: %q", text)
		}
	}
	// And no size. The check is on the health page of every site owner,
	// and a deployment's backup size is the other customers' volume too
	// (W1c withheld it for that reason).
	if strings.Contains(r.Detail, "700") || strings.Contains(r.Detail, "MB") {
		t.Errorf("the detail carries a size: %q", r.Detail)
	}
}

func TestBackupCheck_ABackupThatExistsIsNamed(t *testing.T) {
	f := newBackupFixture(t)
	f.policy()
	f.take(3*24*time.Hour, "")
	f.backup(3*24*time.Hour, "present", nil, "")

	got := f.check()
	assertNotTheOldSentence(t, got)
	if got.Status != CheckPass {
		t.Errorf("a deployment with a backup on disk is %s, want pass: %q", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, f.now.Add(-3*24*time.Hour).Format("2006-01-02")) {
		t.Errorf("the detail does not give the backup's date: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "doğrulanmadı") {
		t.Errorf("an unchecked backup is not said to be unchecked: %q", got.Detail)
	}

	// Checked and clean: the same pass, without the caveat.
	f.exec(`UPDATE ` + backupCheckSchema + `.panel_backups SET verified_at = now()`)
	got = f.check()
	assertNotTheOldSentence(t, got)
	if got.Status != CheckPass || strings.Contains(got.Detail, "doğrulanmadı") {
		t.Errorf("a checked backup: %s %q", got.Status, got.Detail)
	}
}

func TestBackupCheck_TheNewestBackupIsTheOneJudged(t *testing.T) {
	f := newBackupFixture(t)
	f.policy()
	clean := f.now
	f.backup(10*24*time.Hour, "present", &clean, "")
	f.backup(1*24*time.Hour, "present", &clean, "tablo eksik: panel_users")

	got := f.check()
	if got.Status != CheckWarn {
		t.Errorf("the newest backup failed its check and the result is %s: %q", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, f.now.Add(-24*time.Hour).Format("2006-01-02")) {
		t.Errorf("the warning does not name the broken backup's date: %q", got.Detail)
	}
}

func TestBackupCheck_ATakeThatFailedAfterTheLastBackupIsSaid(t *testing.T) {
	f := newBackupFixture(t)
	f.policy()
	f.backup(5*24*time.Hour, "present", nil, "")
	f.take(time.Hour, "backup: the target filesystem is short by 2.1 GB")

	got := f.check()
	if got.Status != CheckWarn {
		t.Errorf("the last attempt failed and the result is %s: %q", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, f.now.Add(-5*24*time.Hour).Format("2006-01-02")) {
		t.Errorf("the warning does not say which backup there still is: %q", got.Detail)
	}

	// A failure before the backup is history, not news.
	f2 := newBackupFixture(t)
	f2.policy()
	f2.take(5*24*time.Hour, "backup: the target filesystem is short by 2.1 GB")
	f2.backup(time.Hour, "present", nil, "")
	if got := f2.check(); got.Status != CheckPass {
		t.Errorf("a failure older than the newest backup turned it into %s: %q", got.Status, got.Detail)
	}
}

func TestBackupCheck_NoDirectoryIsNamedAsSuch(t *testing.T) {
	f := newBackupFixture(t)
	f.policy()
	f.take(time.Minute, backup.ErrNotConfigured.Error())

	got := f.check()
	assertNotTheOldSentence(t, got)
	if got.Status != CheckWarn {
		t.Errorf("status %s, want warn", got.Status)
	}
	if !strings.Contains(got.Fix, "[backup]") || !strings.Contains(got.Fix, "upgrader.toml") {
		t.Errorf("the fix does not say where the directory is set: %q", got.Fix)
	}

	// A newer request of another kind - a check of some other file - is
	// not the answer to "did the last attempt to take one work".
	f.exec(`INSERT INTO ` + backupCheckSchema + `.panel_backup_requests
		(actor_kind, kind, sets, target_id, state, finished_at)
		VALUES ('user', 'dogrula', '{}', 1, 'succeeded', now())`)
	if again := f.check(); again.Detail != got.Detail {
		t.Errorf("a newer check request hid the failed take: %q", again.Detail)
	}

	// Another failure is not reported as the missing directory, and is
	// reported as a failure.
	f2 := newBackupFixture(t)
	f2.policy()
	f2.take(time.Minute, "backup: the target filesystem is short by 2.1 GB")
	got2 := f2.check()
	if got2.Status != CheckWarn || got2.Detail == got.Detail {
		t.Errorf("a full disk reads the same as no directory: %q", got2.Detail)
	}
	if !strings.Contains(got2.Detail, "başarısız") {
		t.Errorf("a failed request is not said to have failed: %q", got2.Detail)
	}
}

func TestBackupCheck_OnlyMissingFilesIsNotABackup(t *testing.T) {
	f := newBackupFixture(t)
	f.policy()
	f.backup(2*24*time.Hour, "missing", nil, "")

	got := f.check()
	if got.Status != CheckWarn {
		t.Errorf("a catalogue whose only file is gone is %s: %q", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "diskte") {
		t.Errorf("the warning does not say the file is gone from the disk: %q", got.Detail)
	}
}

func TestBackupCheck_NothingYet(t *testing.T) {
	// The upgrader has never run: its policy row is not there.
	f := newBackupFixture(t)
	got := f.check()
	assertNotTheOldSentence(t, got)
	if got.Status != CheckWarn || !strings.Contains(got.Fix, "crucible-upgrader.timer") {
		t.Errorf("an upgrader that never ran: %s %q / %q", got.Status, got.Detail, got.Fix)
	}

	// It has run, and nobody has asked for a backup.
	f2 := newBackupFixture(t)
	f2.policy()
	got2 := f2.check()
	assertNotTheOldSentence(t, got2)
	if got2.Status != CheckWarn || !strings.Contains(got2.Fix, "Sağlık") {
		t.Errorf("no backup yet: %s %q / %q", got2.Status, got2.Detail, got2.Fix)
	}
	if got.Detail == got2.Detail {
		t.Errorf("an upgrader that never ran and one that ran read the same: %q", got.Detail)
	}
}
