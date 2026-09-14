//go:build integration

package logsink

import (
	"context"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The owner of this table cannot write a log line in another service's
// name.
//
// The same defect as internal/heartbeat's, on the table where it is
// worse. A heartbeat row is a claim about liveness; a log row is the
// record an operator reads to find out what a service did. A service
// column its writer can choose makes the column decoration.
//
// cmd/upgrader attaches this very sink to the schema_admin pool, so the
// role that could forge the column is not hypothetical - it is the one
// component that writes here as the table's owner.
func TestTheOwnerCannotWriteAnotherServicesLogLine(t *testing.T) {
	ctx := context.Background()

	// This suite shares the development database, so the schema is
	// applied first: the claim is about this tree's schema rather than
	// about the shape the database was left in.
	admin := testdb.Admin(t)
	if _, err := admin.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("applying the log schema: %v", err)
	}

	owner := testdb.Pool(t, testdb.SchemaAdmin)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_logs WHERE category = 'l6-force-probe'`); err != nil {
			t.Errorf("removing the rows this test wrote: %v", err)
		}
	})

	_, err := owner.Exec(ctx, `
		INSERT INTO panel_logs (at, service, level, category, message)
		VALUES (now(), 'collector', 'error', 'l6-force-probe', 'the collector did not write this')`)
	if err == nil {
		t.Error("schema_admin wrote a panel_logs row labelled 'collector'.\n" +
			"The write policy says service = current_user, and without FORCE ROW LEVEL " +
			"SECURITY the table's owner is not subject to it - so the one column that " +
			"says who did something could be chosen by whoever wrote the row.")
	} else if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("the insert failed for some other reason than the policy, so this test "+
			"is not measuring the policy: %v", err)
	}

	if _, err := owner.Exec(ctx, `
		INSERT INTO panel_logs (at, service, level, category, message)
		VALUES (now(), $1, 'info', 'l6-force-probe', 'own line')`, testdb.SchemaAdmin); err != nil {
		t.Errorf("schema_admin cannot write its own log line either: %v.\n"+
			"cmd/upgrader logs through this sink on the schema_admin pool, so this "+
			"failing would mean an upgrade whose log lines silently stop being "+
			"written - the run nobody can explain afterwards.", err)
	}
}
