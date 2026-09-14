//go:build integration

package heartbeat

import (
	"context"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The owner of this table cannot write another service's row.
//
// # What this is about
//
// The write policy says service = current_user, and a policy does
// nothing to the table's owner unless the table forces it. The owner
// used to be described here as "the installing superuser ... not a role
// any service connects as", and both halves had stopped being true:
// grants.sql transfers every table to schema_admin, and cmd/upgrader
// connects as schema_admin with the log sink attached to that pool.
//
// So this asserts the thing the schema's own comment promised and the
// schema did not deliver. Measured before the fix: the insert below was
// accepted.
//
// # Both directions, and why the second one is not padding
//
// Forcing row security on a table is a change that can break the owner
// entirely, and an upgrade that stops schema_admin writing its own log
// and heartbeat rows would be a worse defect than the one being fixed -
// silent, and visible only as a service that appears never to have
// started. So the legitimate write is asserted beside the forged one.
func TestTheOwnerCannotWriteAnotherServicesHeartbeat(t *testing.T) {
	ctx := context.Background()

	// The schema is applied first, by the superuser, because this suite
	// runs against the shared development database and the property
	// under test is a property of *this tree's* schema. Without it the
	// test would report on whatever shape the database happened to be
	// left in, and a schema mutation would change nothing it sees.
	admin := testdb.Admin(t)
	if _, err := admin.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("applying the heartbeat schema: %v", err)
	}

	owner := testdb.Pool(t, testdb.SchemaAdmin)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE service = $1`, testdb.SchemaAdmin); err != nil {
			t.Errorf("removing the row this test wrote: %v", err)
		}
	})

	_, err := owner.Exec(ctx, `
		INSERT INTO service_heartbeat (service, version, started_at, beat_at, counters)
		VALUES ('collector', 'not the collector', now(), now(), '{}'::jsonb)`)
	if err == nil {
		t.Error("schema_admin wrote a heartbeat row labelled 'collector'.\n" +
			"That is the row the health page reads to say the collector is alive, and " +
			"the write policy claims only the collector can write it. Without FORCE ROW " +
			"LEVEL SECURITY the owner bypasses the policy, and the owner is the role " +
			"holding the deployment's DDL credential.")
	} else if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("the insert failed for some other reason than the policy, so this test "+
			"is not measuring the policy: %v", err)
	}

	if _, err := owner.Exec(ctx, `
		INSERT INTO service_heartbeat (service, version, started_at, beat_at, counters)
		VALUES ($1, 'own row', now(), now(), '{}'::jsonb)`, testdb.SchemaAdmin); err != nil {
		t.Errorf("schema_admin cannot write its own heartbeat row either: %v.\n"+
			"Forcing row security must bound the owner to its own row, not lock it out "+
			"of the table - every writer labels the row from SELECT current_user, so "+
			"its own row is the only one it ever writes.", err)
	}
}
