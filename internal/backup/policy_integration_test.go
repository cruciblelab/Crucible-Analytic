//go:build integration

package backup_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The age limit, from the process that knows it to the page that says it.
//
// # Why this row exists at all
//
// `[backup] keep_days` is in upgrader.toml, which carries the only DSN in
// this deployment that can run DDL - so the panel's account cannot read
// that file and must never be able to. The upgrader writes what it read;
// the panel reads the record. Same answer F1c gave for the device number.
//
// # What is asserted, and why the negative one is the point
//
// The round trip is the easy half. The half worth having is that the
// panel cannot *write* it: the limit is the operator's decision, in a
// file only root edits, and a panel that could write this row could tell
// a customer a limit that is not in force - which is worse than telling
// them nothing at all.

// TestTheAgeLimitReachesThePanelAndNothingElseCanSetIt.
func TestTheAgeLimitReachesThePanelAndNothingElseCanSetIt(t *testing.T) {
	ctx := context.Background()
	answers := testdb.Pool(t, testdb.SchemaAdmin)
	asks := testdb.Pool(t, testdb.Panel)

	// Whatever this database already said, restored afterwards. The row
	// is one row for the whole deployment, so a test that left its own
	// number behind would be deciding what the next suite reads.
	before, err := backup.ReadPolicy(ctx, answers)
	if err != nil {
		t.Fatalf("reading the policy this database started with: %v", err)
	}
	t.Cleanup(func() {
		if err := backup.NotePolicy(context.Background(), answers, before.KeepDays); err != nil {
			t.Errorf("restoring the policy: %v", err)
		}
	})

	for _, want := range []int{90, 0, 365} {
		if err := backup.NotePolicy(ctx, answers, want); err != nil {
			t.Fatalf("NotePolicy(%d): %v", want, err)
		}
		got, err := backup.ReadPolicy(ctx, asks)
		if err != nil {
			t.Fatalf("the panel could not read the policy: %v", err)
		}
		if got.KeepDays != want {
			t.Errorf("the panel reads keep_days = %d, the upgrader wrote %d",
				got.KeepDays, want)
		}
		if !got.Known() {
			t.Error("a policy the upgrader has written reports itself as unknown, so " +
				"the page would say nobody has been told - about a limit somebody set")
		}
		if got.KeepsForever() != (want == 0) {
			t.Errorf("keep_days = %d reports KeepsForever = %v", want, got.KeepsForever())
		}
	}

	// And the half that matters.
	for _, sql := range []string{
		`UPDATE panel_backup_policy SET keep_days = 1 WHERE id = 1`,
		`INSERT INTO panel_backup_policy (id, keep_days) VALUES (1, 1)
		   ON CONFLICT (id) DO UPDATE SET keep_days = 1`,
		`DELETE FROM panel_backup_policy WHERE id = 1`,
	} {
		_, err := asks.Exec(ctx, sql)
		if err == nil {
			t.Errorf("the panel's role ran %q.\n"+
				"The age limit is the operator's decision, written in a file only "+
				"root edits. A panel that can write this row can tell a customer a "+
				"limit that is not in force", strings.Join(strings.Fields(sql), " "))
			continue
		}
		// Which refusal, not merely that there was one. A unique-index
		// violation or a missing table would satisfy "err != nil" while
		// saying nothing about privilege, and this repository has paid
		// for that shortcut before.
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "permission denied") &&
			!strings.Contains(msg, "row-level security") {
			t.Errorf("%q was refused, but not for want of privilege: %v",
				strings.Join(strings.Fields(sql), " "), err)
		}
	}
}
