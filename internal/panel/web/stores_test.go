package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// What the narrow interfaces in stores.go are for.
//
// Every test in this file exercises a section's logic with no database,
// no HTTP request and no server configuration - a fake with one method
// and a *Server that has never been near Postgres. Before the seams
// existed none of these could be written at all: the field was typed
// *panel.Store, so reaching any of these branches meant arranging a real
// database into the state that produces it.
//
// Two of the three states below cannot be arranged in a real database at
// all without breaking something on purpose: a size query that fails, and
// a catalogue row whose file has been deleted underneath it. Those are
// precisely the branches that decide what a person is told when
// something has gone wrong, which is when this page is read.

// quietServer is a panel with a logger and nothing else. It is what the
// area functions are entitled to need.
func quietServer() *Server {
	return &Server{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func testLang(t *testing.T) *ui.Language {
	t.Helper()
	cats, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	return cats.Base()
}

// fakeBackupReader answers the one question the backup section asks.
type fakeBackupReader struct {
	status panel.BackupStatus
	err    error
}

func (f fakeBackupReader) BackupStatus(context.Context, panel.Access) (panel.BackupStatus, error) {
	return f.status, f.err
}

// TestTheBackupTotalLeavesOutFilesThatAreGone.
//
// # What goes wrong without it
//
// The catalogue keeps a row for a backup whose file has since been
// deleted - the sweep marks it "missing" rather than removing it, so the
// history stays honest about what was taken. The section still lists it,
// and must not add its size to the total.
//
// The total is drawn beside the disk figures. A total that counted files
// the disk no longer holds would be a number about storage that storage
// disagrees with, and the person reading it is deciding whether there is
// room for another backup.
//
// # Why this could not be tested before
//
// Reaching the branch means a panel_backups row in state "missing",
// which the runner only writes after finding the file gone. Arranging
// that against a real database meant taking a backup, deleting the file
// behind it and running the sweep - three moving parts to test one
// addition.
func TestTheBackupTotalLeavesOutFilesThatAreGone(t *testing.T) {
	s := quietServer()
	lang := testLang(t)

	db := fakeBackupReader{status: panel.BackupStatus{
		Allowed: true,
		Backups: []backup.Backup{
			{TakenAt: time.Now(), Bytes: 1000, State: "ok"},
			{TakenAt: time.Now(), Bytes: 9_000_000, State: "missing"},
			{TakenAt: time.Now(), Bytes: 500, State: "ok"},
		},
	}}

	section, msg := s.backupStatusFor(context.Background(), db, lang, panel.Access{})
	if msg != "" {
		t.Fatalf("the section failed to build: %s", msg)
	}
	if len(section.Backups) != 3 {
		t.Fatalf("the list dropped a row: %d rows, want 3", len(section.Backups))
	}
	if !section.Backups[1].Missing {
		t.Error("the missing file is not marked missing, so nothing tells the reader why")
	}
	if section.TotalBytes != 1500 {
		t.Errorf("total %d bytes, want 1500: a file that is gone is being counted",
			section.TotalBytes)
	}
}

// TestAnUnreadableBackupListDrawsNothingRatherThanAnEmptyOne.
//
// An empty catalogue and an unreadable one are the same picture unless
// the section says which. "There are no backups" said to somebody who has
// backups is the one sentence this page must never print by accident.
func TestAnUnreadableBackupListDrawsNothingRatherThanAnEmptyOne(t *testing.T) {
	s := quietServer()
	lang := testLang(t)

	db := fakeBackupReader{err: errors.New("connection refused")}
	section, msg := s.backupStatusFor(context.Background(), db, lang, panel.Access{})

	if msg == "" {
		t.Fatal("a failed read produced no sentence for the reader")
	}
	if msg == "saglik.yedek.okunamadi" {
		t.Error("the catalog has no entry for this, so the reader gets a key")
	}
	if len(section.Sets) != 0 || section.Allowed {
		t.Error("a failed read still drew a form; the section must be absent, not empty")
	}
}

// fakeDiskStore answers the questions the storage section asks.
type fakeDiskStore struct {
	bytes int64
	err   error

	// backups is what the backups occupy per filesystem, and unplaced is
	// what could not be attributed to one.
	backups   map[int64]int64
	unplaced  int64
	backupErr error
}

func (f fakeDiskStore) DatabaseBytes(context.Context) (int64, error) { return f.bytes, f.err }

func (f fakeDiskStore) BackupBytesByDevice(context.Context) (map[int64]int64, int64, error) {
	return f.backups, f.unplaced, f.backupErr
}

// TestADatabaseSizeThatCannotBeReadIsNotADatabaseOfZeroBytes.
//
// # What goes wrong without it
//
// healthDisk carries DatabaseKnown beside DatabaseBytes precisely so the
// template can tell "could not read it" from "it is empty". Drop the
// flag and a failed query renders as a database occupying no space at
// all - a number somebody would act on, on the page they opened because
// they suspected something was wrong.
//
// # Why this could not be tested before
//
// The branch needs pg_database_size to fail. Against a real database
// that means stopping Postgres mid-test or revoking a grant; here it is
// a struct field.
func TestADatabaseSizeThatCannotBeReadIsNotADatabaseOfZeroBytes(t *testing.T) {
	s := quietServer()
	lang := testLang(t)

	failed := s.healthDiskSection(context.Background(),
		fakeDiskStore{err: errors.New("server closed the connection")}, lang)
	if failed.DatabaseKnown {
		t.Error("a failed size query reports a known size")
	}
	if failed.DatabaseBytes != 0 {
		t.Errorf("a failed size query produced %d bytes", failed.DatabaseBytes)
	}

	// The other side of the same distinction: a database that really is
	// small must be reported as known, or the flag would just mean "the
	// number is small".
	read := s.healthDiskSection(context.Background(), fakeDiskStore{bytes: 8192}, lang)
	if !read.DatabaseKnown || read.DatabaseBytes != 8192 {
		t.Errorf("a successful read came back as known=%v bytes=%d",
			read.DatabaseKnown, read.DatabaseBytes)
	}
}

// fakeUpgradeReader answers the one question the schema section asks.
type fakeUpgradeReader struct {
	status panel.UpgradeStatus
}

func (f fakeUpgradeReader) UpgradeStatus(context.Context, panel.Access) (panel.UpgradeStatus, error) {
	return f.status, nil
}

// TestTheSchemaSectionAsksForThePasswordOnlyWhenItWouldOpenTheButton.
//
// # What goes wrong without it
//
// AskingForPassword draws a password field. Draw it when the lock is off
// and somebody types the developer password into a form that does not
// need it; draw it for a principal with no entitlement and the form
// promises that a password would let them through, which it would not -
// panel.RequestUpgrade refuses on the capability before it ever looks at
// the authorization.
//
// A form that asks for a password it will not honour is worse than a
// refusal: it teaches somebody to type the developer password into
// places that do not use it.
//
// # Why this could not be tested before
//
// The three inputs are a schema mismatch, a setting and a role. Reaching
// the interesting corner - needed, locked, entitled - meant a database
// whose schema_version had been moved backwards on purpose.
func TestTheSchemaSectionAsksForThePasswordOnlyWhenItWouldOpenTheButton(t *testing.T) {
	s := quietServer()
	lang := testLang(t)
	owner := panel.Access{Role: panel.RoleOwner, Member: true}
	viewer := panel.Access{Role: panel.RoleViewer, Member: true}

	cases := []struct {
		name   string
		status panel.UpgradeStatus
		access panel.Access
		want   bool
	}{
		{"behind, locked, entitled", panel.UpgradeStatus{Needed: true, Locked: true}, owner, true},
		{"behind, unlocked, entitled", panel.UpgradeStatus{Needed: true}, owner, false},
		{"behind, locked, not entitled", panel.UpgradeStatus{Needed: true, Locked: true}, viewer, false},
		{"current, locked, entitled", panel.UpgradeStatus{Locked: true}, owner, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			section, msg := s.upgradeStatusFor(context.Background(),
				fakeUpgradeReader{status: c.status}, lang, c.access)
			if msg != "" {
				t.Fatalf("the section failed to build: %s", msg)
			}
			if section.AskingForPassword != c.want {
				t.Errorf("asking for the password = %v, want %v",
					section.AskingForPassword, c.want)
			}
		})
	}
}

// TestOwnerAndViewerDisagreeAboutTheUpgradeButton guards the assumption
// the table above rests on.
//
// Both rows for "not entitled" would pass if a viewer happened to have
// the capability, and the test would then be checking nothing. This
// states the premise separately so a change to the role table breaks
// here, with a sentence, rather than quietly turning four cases into
// two.
func TestOwnerAndViewerDisagreeAboutTheUpgradeButton(t *testing.T) {
	if !(panel.Access{Role: panel.RoleOwner, Member: true}).Can(panel.CapManageSettings) {
		t.Error("an owner cannot manage settings, so the entitled cases above test nothing")
	}
	if (panel.Access{Role: panel.RoleViewer, Member: true}).Can(panel.CapManageSettings) {
		t.Error("a viewer can manage settings, so the unentitled cases above test nothing")
	}
}

// TestBackupsThatBelongToNoDrawnDiskAreStillCounted.
//
// # What goes wrong without it
//
// A backup row carries the filesystem it was written to. Three things
// leave that unknown: a backup taken before the column existed, one
// whose filesystem could not be read, and one written to a directory the
// panel was never configured with.
//
// Silently dropping those bytes would make this section disagree with
// the backup section's own total on the same page, for a reason no
// reader could see. Adding them to a bar would put bytes on a disk that
// may not hold them.
//
// So they are neither: they are a figure of their own, and the page says
// what it means.
func TestBackupsThatBelongToNoDrawnDiskAreStillCounted(t *testing.T) {
	s := quietServer()
	lang := testLang(t)

	got := s.healthDiskSection(context.Background(), fakeDiskStore{
		backups:  map[int64]int64{99: 4096},
		unplaced: 12345,
	}, lang)

	if got.UnplacedBackupBytes != 12345 {
		t.Errorf("unplaced backups came back as %d bytes, want 12345",
			got.UnplacedBackupBytes)
	}
}

// TestAnUnreadableBackupTotalCostsOnlyTheSegment.
//
// Every part of the health page fails independently, which is the page's
// whole reason for existing. The backups are one number on top of a
// measurement; failing to read them must not take the disk figures with
// them, because the disk figures are what somebody opened the page for.
func TestAnUnreadableBackupTotalCostsOnlyTheSegment(t *testing.T) {
	s := quietServer()
	lang := testLang(t)

	got := s.healthDiskSection(context.Background(), fakeDiskStore{
		bytes:     8192,
		backupErr: errors.New("connection refused"),
	}, lang)

	if !got.DatabaseKnown || got.DatabaseBytes != 8192 {
		t.Errorf("a failed backup total took the database size with it: known=%v bytes=%d",
			got.DatabaseKnown, got.DatabaseBytes)
	}
	if got.UnplacedBackupBytes != 0 {
		t.Errorf("a failed read reported %d unplaced bytes", got.UnplacedBackupBytes)
	}
}

// The configuration set is offered only where there is a password to
// ask for.
//
// # Why the gate decides whether the box exists
//
// A secrets backup needs the developer password, and that password
// comes from a config file rather than the database - so a deployment
// with no [developer] section cannot ever produce one. A checkbox whose
// only possible outcome is a refusal is worse than no checkbox: it
// tells the customer the feature is theirs and then says no.
//
// The direction matters more than the tidiness. This is the fail-closed
// half of the gate: with no gate configured, the panel does not offer
// the one backup that carries every credential this deployment has.
func TestTheConfigurationIsOfferedOnlyWhenThereIsAPasswordToAskFor(t *testing.T) {
	lang := testLang(t)
	db := fakeBackupReader{status: panel.BackupStatus{Allowed: true}}

	// The gate this deployment has, built from a real argon2id hash so
	// nothing here stands in for the mechanism.
	hash, err := argon2id.Hash("test-gelistirici-sifresi-uzun")
	if err != nil {
		t.Fatal(err)
	}
	gate, err := devgate.New(devgate.Config{PasswordHash: hash}, devgate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// And one with no password at all, which is what every machine
	// starts as.
	shut, err := devgate.New(devgate.Config{}, devgate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		gate *devgate.Gate
		want bool
	}{
		{name: "a developer password is configured", gate: gate, want: true},
		{name: "no developer password", gate: shut, want: false},
		{name: "no gate at all", gate: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := quietServer()
			s.Gate = tc.gate

			section, msg := s.backupStatusFor(context.Background(), db, lang, panel.Access{})
			if msg != "" {
				t.Fatalf("the section failed to build: %s", msg)
			}
			if section.AskingForPassword != tc.want {
				t.Errorf("AskingForPassword is %v, want %v", section.AskingForPassword, tc.want)
			}

			var offered []string
			for _, set := range section.Sets {
				if set.Secrets {
					offered = append(offered, set.Name)
				}
			}
			if tc.want && len(offered) != 1 {
				t.Errorf("the configuration is not offered (%d sets, none of them it), "+
					"so a deployment that can take one cannot ask", len(section.Sets))
			}
			if !tc.want && len(offered) != 0 {
				t.Errorf("the configuration is offered as %v on a deployment whose gate "+
					"is shut; pressing it could only ever be refused", offered)
			}
			// The data sets are offered either way. The gate decides
			// one box, not the section: a deployment with no developer
			// password still takes backups, and always has.
			if len(section.Sets) < 2 {
				t.Errorf("only %d sets offered; the data backup belongs to the customer "+
					"and is not behind this gate", len(section.Sets))
			}
		})
	}
}

// The person who ticked the box must not be shown an unticked one after
// a refusal. Worth pinning because the echo loop runs over the sets the
// section offers, and the configuration set is now sometimes absent
// from that list.
func TestARefusedChoiceIsEchoedBack(t *testing.T) {
	lang := testLang(t)
	s := quietServer()
	db := fakeBackupReader{status: panel.BackupStatus{Allowed: true}}

	section, msg := s.backupStatusFor(context.Background(), db, lang, panel.Access{})
	if msg != "" {
		t.Fatalf("the section failed to build: %s", msg)
	}
	// Default: the panel set, which is what a first press should take.
	var checked []string
	for _, set := range section.Sets {
		if set.Checked {
			checked = append(checked, set.Name)
		}
	}
	if len(checked) != 1 || checked[0] != backup.SetPanel {
		t.Errorf("the default choice is %v, want [%s]. The small set is the one that "+
			"cannot be rebuilt, and a default including the traffic makes the first "+
			"press on a large deployment the one refused for space", checked, backup.SetPanel)
	}
}
