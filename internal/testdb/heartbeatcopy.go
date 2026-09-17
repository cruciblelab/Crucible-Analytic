//go:build integration

package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HeartbeatCopy is a private service_heartbeat for one suite.
//
// # Why a copy rather than the real table
//
// Four suites write real heartbeat rows into the shared database -
// internal/heartbeat twice, internal/relupdate, internal/panel/web - and
// none of them is about the ip_token_key_state column, so all of them
// leave it empty. `go test ./...` runs packages in parallel.
//
// That is fine until a suite needs the table to say something. The panel
// refuses privacy.ip_storage = "full" unless every address writer
// reports a usable key, and the setup wizard reports the same fact, so
// both have to be able to set up a deployment where the collector holds
// one. Seeded into the shared table, that row is overwritten mid-test by
// a package with no interest in it, and the failure surfaces here, as a
// product defect, in whichever run lost the race.
//
// The alternative was an eighth advisory lock, serialising four suites
// against each other over a column three of them never mention. This is
// the shape internal/settings' empty-table fixture already uses for the
// same reason, and it disturbs nobody: every other suite runs with
// search_path = public and cannot see this schema at all.
//
// # Why it lives in this package
//
// Two suites need it - internal/panel for the settings gate,
// internal/panel/preflight for the wizard's check - and they may not
// share a package: preflight deliberately imports nothing from the
// panel, and a test asserts it. Written twice, the two would be two
// copies of one fixture, and this project's rule is that two copies are
// acceptable only when something compares them.
//
// # Why the state is a string
//
// internal/heartbeat's TokenKeyState is the typed form, and this package
// cannot import it: that package's own tests import this one, so the
// dependency would be a cycle through the package under test. A string is
// also the more honest parameter for a fixture that writes a column -
// callers pass string(heartbeat.TokenKeyPresent) and nothing is lost.
type HeartbeatCopy struct {
	t      *testing.T
	admin  *pgxpool.Pool
	schema string
}

// NewHeartbeatCopy builds the schema and the table.
//
// Call it *before* the pool that will read it, so its cleanup runs
// afterwards: t.Cleanup is last-in-first-out, and dropping a schema out
// from under a live pool leaves that pool's statement cache pointing at
// nothing.
//
// The schema name is the caller's, so two suites running in parallel
// against one database do not share one. It is dropped first in case a
// previous run died - a fixture that fails before any assertion is a
// fixture that reports a defect it never looked for.
func NewHeartbeatCopy(t *testing.T, schema string) *HeartbeatCopy {
	t.Helper()
	ctx := context.Background()
	admin := Admin(t)

	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
		// The shape is derived - LIKE ... INCLUDING ALL - rather than
		// typed out here. A hand-written copy would be correct the day it
		// was written and correct only by accident afterwards.
		//
		// What it does not inherit is row-level security: LIKE copies
		// columns, defaults, constraints and indexes, not policies. That
		// is wanted. The policy says a service may write only its own
		// row, its own suite measures it, and a fixture has to be able to
		// say what any service reported.
		`CREATE TABLE ` + schema + `.service_heartbeat (LIKE public.service_heartbeat INCLUDING ALL)`,
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + Panel,
		`GRANT SELECT ON ` + schema + `.service_heartbeat TO ` + Panel,
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

	return &HeartbeatCopy{t: t, admin: admin, schema: schema}
}

// DSN is a connection string that reads the copy.
//
// The base is taken as an argument rather than built here because the two
// callers spell it themselves - each suite's own constant carries the
// reason it connects as the role it does. The base must have no query
// string of its own; none of them has.
func (h *HeartbeatCopy) DSN(base string) string {
	return base + "?options=-csearch_path%3D" + h.schema + ",public"
}

// Reaches fails the test unless a pool resolves service_heartbeat to the
// copy.
//
// Without it a suite could pass by reading the shared table, and would
// then be measuring nothing about the rows it seeded. A fixture that does
// not reach what the code reads is a fixture testing the code's other
// input.
func (h *HeartbeatCopy) Reaches(pool *pgxpool.Pool) {
	h.t.Helper()
	var where string
	if err := pool.QueryRow(context.Background(),
		`SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = 'service_heartbeat'::regclass`).Scan(&where); err != nil {
		h.t.Fatalf("resolving service_heartbeat: %v", err)
	}
	if where != h.schema {
		h.t.Fatalf("this pool resolves service_heartbeat to %q, not to the fixture copy in "+
			"%q; every row seeded here is invisible to the code under test", where, h.schema)
	}
}

// Says records what one service reported about its IP token key.
//
// The service name is a database role, because that is what the real
// table is keyed by. It is not checked against pg_roles on purpose: one
// of the cases worth testing is a heartbeat row that outlived its role.
func (h *HeartbeatCopy) Says(service, state string) {
	h.t.Helper()
	if _, err := h.admin.Exec(context.Background(), `
		INSERT INTO `+h.schema+`.service_heartbeat
		    (service, version, started_at, beat_at, counters, ip_token_key_state)
		VALUES ($1, 'fixture', now(), now(), '{}'::jsonb, $2)
		ON CONFLICT (service) DO UPDATE SET ip_token_key_state = EXCLUDED.ip_token_key_state,
		    beat_at = now()`,
		service, state); err != nil {
		h.t.Fatalf("recording %s = %q: %v", service, state, err)
	}
}

// Forget removes one service's row.
func (h *HeartbeatCopy) Forget(service string) {
	h.t.Helper()
	if _, err := h.admin.Exec(context.Background(),
		`DELETE FROM `+h.schema+`.service_heartbeat WHERE service = $1`, service); err != nil {
		h.t.Fatalf("clearing %s: %v", service, err)
	}
}

// Silence empties the table: a deployment where nothing has started yet.
func (h *HeartbeatCopy) Silence() {
	h.t.Helper()
	if _, err := h.admin.Exec(context.Background(),
		`DELETE FROM `+h.schema+`.service_heartbeat`); err != nil {
		h.t.Fatalf("emptying the fixture: %v", err)
	}
}
