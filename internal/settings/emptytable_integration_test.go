//go:build integration

package settings

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// A successful read that comes back empty empties the cache.
//
// # Why this test did not exist, and what it cost
//
// Refresh replaces the cache rather than merging into it, and the
// comment beside that line explains why at length: the panel deletes a
// row instead of storing a value that means "default", so a setting that
// has gone away has to go away here too. Merging would mean a customer
// could lower a limit and never raise it again.
//
// Half of it was measured. TestSource_ADeletedSettingGoesBackToTheDefault
// covers the ordinary case - one row deleted out of several - and it
// catches a Refresh that *merges*. What it cannot reach is the boundary:
// wrapping the assignment in `if len(fresh) > 0` passes that test,
// because its table still has rows in it. That mutation was the one left
// standing after C3, and it stayed standing for months, because reaching
// it needs the table to come back with *no rows at all* - and
// panel_settings is shared by three integration suites that each keep
// rows in it. All three would have had to be empty at the same moment.
//
// Which is not a hypothetical shape: it is what a deployment looks like
// before anybody has changed a setting, and what one looks like again
// after the customer clears the last one they had changed.
//
// # The fixture, and why it is not a second table
//
// A schema of this test's own, holding a table created with
// `LIKE public.panel_settings INCLUDING ALL` - so its shape is *derived*
// from the real one rather than typed out here. A hand-written copy
// would be correct the day it was written and then only correct by
// accident, which is the failure this project has already paid for in a
// fixture that hand-wrote a wire format.
//
// The Source's pool puts that schema first on its search_path, so the
// unqualified `panel_settings` in Refresh resolves to it. Nothing else
// in the cluster can see it: every other suite runs with search_path =
// public, so emptying this table is invisible to them - which is the
// whole point, because emptying the real one is not.
//
// Writes go in as the superuser and reads come out as panel_user, which
// is also how it works in production: the panel writes, a service reads.
func TestSource_AnEmptyTableEmptiesTheCache(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)

	const schema = "ca_settings_empty"
	for _, sql := range []string{
		// DROP first: a run that died leaves the schema behind, and a
		// fixture that fails before any assertion is a fixture that
		// reports a product defect it never looked for.
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
		`CREATE TABLE ` + schema + `.panel_settings (LIKE public.panel_settings INCLUDING ALL)`,
		`GRANT USAGE ON SCHEMA ` + schema + ` TO panel_user`,
		`GRANT SELECT ON ` + schema + `.panel_settings TO panel_user`,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("dropping %s: %v", schema, err)
		}
	})

	pool, err := pgxpool.New(ctx,
		testDatabaseURL+"?options=-csearch_path%3D"+schema+",public")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// The fixture reaches the table the Source will read, and not the
	// real one. Without this the test could pass by reading public's
	// rows - and it would then be measuring nothing about an empty
	// table.
	var where string
	if err := pool.QueryRow(ctx,
		`SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = 'panel_settings'::regclass`).Scan(&where); err != nil {
		t.Fatalf("resolving panel_settings: %v", err)
	}
	if where != schema {
		t.Fatalf("this pool resolves panel_settings to %q, not to the empty copy in %q",
			where, schema)
	}

	const key = "test.settings.empty-table"
	write := func(value string) {
		t.Helper()
		if _, err := admin.Exec(ctx, `
			INSERT INTO `+schema+`.panel_settings (scope, site_id, key, value)
			VALUES ('global', '', $1, $2::jsonb)
			ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
			key, value,
		); err != nil {
			t.Fatalf("writing %s: %v", key, err)
		}
	}

	// ---- one row: the stored value is in force ----
	//
	// First, because the claim is that an empty read *replaces* what was
	// there. A Source that had never read a value could satisfy every
	// assertion below while keeping stale values forever.
	write(`"stored"`)
	src := New(ctx, pool, Config{Interval: time.Minute})
	if !src.Loaded() {
		t.Fatal("the source did not load from its own schema")
	}
	if got := src.String(key, "", "default", nil); got != "stored" {
		t.Fatalf("with the row present the value is %q, want \"stored\"", got)
	}

	// ---- no rows: the value goes away ----
	if _, err := admin.Exec(ctx, `DELETE FROM `+schema+`.panel_settings`); err != nil {
		t.Fatalf("emptying the table: %v", err)
	}
	if err := src.Refresh(ctx); err != nil {
		t.Fatalf("Refresh over an empty table failed: %v", err)
	}
	if !src.Loaded() {
		t.Error("an empty table left the source reporting that it never loaded. " +
			"Zero rows is an answer, not a failure: it is what a deployment that " +
			"has cleared its last setting looks like.")
	}
	if got := src.String(key, "", "default", nil); got != "default" {
		t.Errorf("after the row was deleted the value is still %q.\n"+
			"A successful read with no rows has to empty the cache. Keeping the "+
			"old values would mean a customer who clears a setting never gets the "+
			"default back - and for the last setting they clear, never gets it "+
			"back at all.", got)
	}
	if _, ok := src.UpdatedAt(key, ""); ok {
		t.Error("the row's timestamp survived the row. Any reader comparing it " +
			"against what it last saw would be told the setting had just changed.")
	}

	// ---- and a failed read is still a different question ----
	//
	// The neighbouring rule, asserted here because this is the only
	// place both can be reached: an empty read empties the cache, a
	// failed read must not. Without this, "replace unconditionally"
	// could be implemented as "replace even when the query failed" and
	// the assertions above would not notice.
	write(`"back"`)
	if err := src.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := src.String(key, "", "default", nil); got != "back" {
		t.Fatalf("the rewritten value is %q", got)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := src.Refresh(cancelled); err == nil {
		t.Error("a refresh on a cancelled context reported success")
	}
	if got := src.String(key, "", "default", nil); got != "back" {
		t.Errorf("a failed refresh changed the value to %q; it must keep the last "+
			"known one", got)
	}
}
