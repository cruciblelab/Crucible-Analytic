//go:build integration

package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The role this API connects as can read the analytics and write
// nothing - asked of the catalogue, not of the grant file.
//
// # The jewel this guards
//
// PLAN.md §3.5, J4, in the owner's words: *"yoken yazamaz ama
// analitiği okuyabilecek güçte olmalı."* Two halves, and a suite that
// checks one of them checks neither - a role with no access at all
// satisfies "cannot write" perfectly.
//
// # Why the catalogue and not release/sql/grants.sql
//
// Because reading the file under-reported, measured on 2026-09-22. A
// grep for lines naming this role next to a write verb found exactly
// one: INSERT on panel_logs. The database, asked the same question,
// answered three privileges on two tables - the heartbeat grant is
// written across two lines, with the verbs above the role names:
//
//	GRANT SELECT, INSERT, UPDATE ON service_heartbeat
//	  TO collector, beacon_writer, analytics_reader, panel_user;
//
// A source scanner that cannot see a wrapped statement is not a
// scanner, and the mistake was mine rather than the file's. It is also
// the second time in this project that a grep reported fewer members
// than existed; the rule earned from the first time was *bir dedektör
// aradığını bulamadığını söylemez.*
//
// Asking the catalogue also covers the privileges the schema files
// grant, the ones an upgrade grants, and anything a hand-run statement
// left behind on a real deployment. The file only describes a fresh
// install.
//
// # Why the exceptions are conditions rather than a note
//
// Two write privileges are deliberate, and PLAN.md's rule is that an
// exemption which can be stated as conditions is not written as a list
// of names. Each one here carries a second, checkable property, and
// that property is the whole reason the privilege is safe:
//
//   - panel_logs INSERT is the log channel a customer with no shell
//     reads. It is safe because the role holds no SELECT on that table:
//     it can append a line about itself and cannot read the log back.
//   - service_heartbeat INSERT and UPDATE let the service write its own
//     row. The grant is deliberately broader than the permission - its
//     own comment in grants.sql says so - and what narrows it is row
//     level security, enabled and FORCED, with a policy keyed on the
//     connecting role. Without that policy the broad grant becomes the
//     real permission, so the policy is not a separate subject: it is
//     this exemption's second half.
//
// The behaviour behind that second half is measured separately, in
// internal/heartbeat's forced-RLS suite, which shows one service cannot
// write another's row. This test holds the structure that suite relies
// on, so that dropping the policy fails here even if that suite is not
// the one somebody ran.
func TestTheReadersRoleCanReadTheAnalyticsAndWriteNothingElse(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Admin(t)

	// key is "table/privilege"; value is why it is allowed to exist.
	expected := map[string]string{
		"panel_logs/INSERT": "the panel's copy of this service's log lines - the " +
			"subset a customer with no shell can read; write-only from here",
		"service_heartbeat/INSERT": "this service's own health row, so the panel can " +
			"tell a busy service from an absent one",
		"service_heartbeat/UPDATE": "the same row on every later beat",
	}

	rows, err := pool.Query(ctx, `
		SELECT table_name, privilege_type
		FROM information_schema.role_table_grants
		WHERE grantee = $1 AND privilege_type <> 'SELECT'
		ORDER BY table_name, privilege_type`, testdb.Reader)
	if err != nil {
		t.Fatalf("reading the role's grants: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var table, priv string
		if err := rows.Scan(&table, &priv); err != nil {
			t.Fatal(err)
		}
		found[table+"/"+priv] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for k := range found {
		if _, ok := expected[k]; !ok {
			t.Errorf("%s holds %s, which is not one of this role's three known "+
				"write privileges.\n"+
				"The token handed to a caller is promised to read and never write. A "+
				"new write privilege is a decision about what that token is, not a "+
				"grant detail: either take it back, or add it here with the reason it "+
				"is safe and the second condition that makes it safe (PLAN.md §3.5 J4).",
				testdb.Reader, k)
		}
	}
	// The other direction: a reason recorded for a privilege that is
	// gone describes code that no longer exists, and the next reader
	// trusts it.
	for k, why := range expected {
		if !found[k] {
			t.Errorf("%s no longer holds %s, but this test still carries the reason "+
				"it was allowed: %q.\nIf the privilege was deliberately taken back, "+
				"remove the entry; if it vanished by accident, the feature behind it "+
				"is broken on every deployment.", testdb.Reader, k, why)
		}
	}

	// Vacuity: nothing found at all means the query, the role name or
	// the grants drifted, and an empty set compares clean against every
	// expectation that is also empty.
	if len(found) == 0 {
		t.Fatalf("no write privilege of any kind was found for %s.\nThree are "+
			"expected. Either this database was not built with release/sql/grants.sql, "+
			"or the question being asked no longer reaches the answer - and a check "+
			"that cannot see a privilege cannot see a new one either.", testdb.Reader)
	}

	// The taken half of the rule. Everything above is "cannot write",
	// and a role with no access at all passes every line of it - which
	// would be a broken deployment reported as a secure one. The rule
	// the owner stated has two halves and this is the second:
	// *analitiği okuyabilecek güçte olmalı.*
	//
	// The two tables named here are the analytics themselves: the
	// collector's side and the beacon's side. They are named rather
	// than derived because the claim is about these two specifically -
	// a role that could read every table in the database would satisfy
	// a derived list and fail the point of the role.
	for _, table := range []string{"traffic_snapshots", "beacon_events"} {
		var canRead bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.role_table_grants
				WHERE grantee = $1 AND table_name = $2
				  AND privilege_type = 'SELECT')`,
			testdb.Reader, table).Scan(&canRead); err != nil {
			t.Fatalf("asking about SELECT on %s: %v", table, err)
		}
		if !canRead {
			t.Errorf("%s cannot SELECT from %s.\n"+
				"Half of this role's definition is that it reads the analytics; a role "+
				"that can write nothing and read nothing satisfies every other "+
				"assertion in this test while serving a customer nothing but errors.",
				testdb.Reader, table)
		}
	}
}

// The second half of each exemption: the property that makes the write
// privilege safe, rather than the sentence saying it is.
func TestEachWritePrivilegeOfTheReadersRoleIsNarrowedBySomething(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Admin(t)

	// panel_logs: safe because this role cannot read it back. A role
	// that could both append and read would hold a channel for moving
	// text between deployments' own sessions; INSERT alone is a report,
	// not a store.
	var canRead bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.role_table_grants
			WHERE grantee = $1 AND table_name = 'panel_logs'
			  AND privilege_type = 'SELECT')`, testdb.Reader).Scan(&canRead); err != nil {
		t.Fatalf("asking about panel_logs SELECT: %v", err)
	}
	if canRead {
		t.Errorf("%s can both INSERT into and SELECT from panel_logs.\n"+
			"The INSERT is excused on the grounds that this role writes a line about "+
			"itself and cannot read the log; with SELECT as well, that reason is no "+
			"longer true and the exemption has to be re-argued.", testdb.Reader)
	}

	// service_heartbeat: safe because row level security is enabled AND
	// forced, and the write policy keys on the connecting role. Enabled
	// without forced is not a rule for the table's owner - and the owner
	// here is a real role with a password in a config file.
	var enabled, forced bool
	if err := pool.QueryRow(ctx, `
		SELECT relrowsecurity, relforcerowsecurity
		FROM pg_class WHERE relname = 'service_heartbeat'`).Scan(&enabled, &forced); err != nil {
		t.Fatalf("asking about service_heartbeat RLS: %v", err)
	}
	if !enabled || !forced {
		t.Errorf("service_heartbeat has row level security enabled=%v forced=%v; "+
			"both have to be true.\nThe grant on this table is deliberately broader "+
			"than the permission - every service may INSERT and UPDATE it - and the "+
			"only thing narrowing that to the writer's own row is the policy. Without "+
			"it, the broad grant is the permission.", enabled, forced)
	}

	// And the policy has to be the one that does the narrowing: it must
	// cover writes and it must decide by the connecting role. A policy
	// named right that tests something else would pass a name check.
	rows, err := pool.Query(ctx, `
		-- polcmd is "char" rather than text, which pgx will not scan
		-- into a string; the cast is the fix, not a formatting choice.
		SELECT polname, polcmd::text, coalesce(pg_get_expr(polqual, polrelid), '')
		FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
		WHERE c.relname = 'service_heartbeat'`)
	if err != nil {
		t.Fatalf("reading service_heartbeat policies: %v", err)
	}
	defer rows.Close()

	var narrowing []string
	var all []string
	for rows.Next() {
		var name, cmd, qual string
		if err := rows.Scan(&name, &cmd, &qual); err != nil {
			t.Fatal(err)
		}
		all = append(all, fmt.Sprintf("%s(cmd=%s)", name, cmd))
		// polcmd '*' is FOR ALL; 'w' is UPDATE, 'a' is INSERT. Any of
		// the three can carry the narrowing, and which one it is should
		// not be this test's business.
		writes := cmd == "*" || cmd == "w" || cmd == "a"
		if writes && strings.Contains(strings.ToUpper(qual), "CURRENT_USER") {
			narrowing = append(narrowing, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(narrowing) == 0 {
		sort.Strings(all)
		t.Errorf("no policy on service_heartbeat both covers writes and decides by "+
			"the connecting role.\nPolicies present: %s\n"+
			"This is what turns a grant every service holds into a permission over "+
			"one row. A policy that reads only is not it, and neither is one that "+
			"decides by a column the writer controls.", strings.Join(all, ", "))
	}
}
