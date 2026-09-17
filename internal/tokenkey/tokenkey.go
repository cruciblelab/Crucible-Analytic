// Package tokenkey answers one question: could this deployment honour
// privacy.ip_storage = "full"?
//
// # The defect that made it a package
//
// The panel gated full mode on a boolean field of its store. The method
// that set the field was called by two tests and by no production code,
// so on every real deployment it was false and full mode could not be
// selected at all - and the setup wizard's check for the key read the
// same field, so it reported "not configured" on every install,
// including the ones that had a key.
//
// A field nobody fills is not a weaker check, it is no check wearing
// one's clothes. The same class as preflight.checkService, found in the
// same week; the invariant written then did not catch this one because
// this was not a Config field but an argument.
//
// # Why the answer comes from the services
//
// The question is not "does a key exist somewhere". It is "can both
// writers of an address produce the same token", and the key lives in
// collector.toml and beacon.toml - files the panel's role cannot read,
// deliberately, because they carry database passwords for roles the
// panel must never hold.
//
// So each service answers for itself, in the one channel that already
// runs from a service to the panel: its heartbeat row. See
// internal/heartbeat/schema.sql for the column and for what may never go
// into it.
//
// # Why this is its own package and not a method on the panel's store
//
// Two readers ask it and they are not allowed to share a package. The
// settings gate lives in internal/panel; the setup wizard's check lives
// in internal/panel/preflight, which deliberately imports nothing from
// the panel - its own comment says so, and the reason is that preflight
// asks questions about *other* roles and would otherwise grow the
// panel's data API by a dozen functions serving one page.
//
// Written twice instead, the two would be two copies of one rule, and
// the drift would be silent in the worse direction: a wizard that says
// "ready" while the gate refuses, or the reverse.
package tokenkey

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// Report is one service's answer.
type Report struct {
	// Service is the database role, which is how the heartbeat table
	// keys a row and therefore who this is.
	Service string
	State   heartbeat.TokenKeyState
}

// Ready reports whether this service could tokenise.
func (r Report) Ready() bool { return r.State == heartbeat.TokenKeyPresent }

// Reports asks every service that can write an address what it says
// about its IP token key.
//
// # Two ways a row qualifies, and why one is not enough
//
// A service that reports present or absent has answered the question,
// and it is counted on that alone. Only the two binaries that write
// addresses set the field - see heartbeat.Options.IPTokenKey - so a row
// that says anything at all is a row from an address writer, whatever
// role it connects as.
//
// The trouble is the third state. A build older than the column reports
// nothing, and so does the read API, which writes no addresses and has
// no opinion. Silence therefore has to be resolved from outside the row,
// or an old collector would be waved through as though it were the read
// API - and that is the one direction that must not be wrong, because it
// ends with full mode selected on a deployment that writes no tokens.
//
// So a silent row qualifies when its role was granted the ability to
// write an address.
//
// # Why that half is derived from privileges
//
// "Which services write addresses" could have been two role names in
// panel.toml, and that is the shape that just failed: a value somebody
// has to keep in step, silent when it is wrong. The database already
// knows, so the list is a question rather than a setting - and if a
// deployment renames its roles, runs a second collector, or grants the
// beacon's writer to something else, the answer follows without an edit.
//
// # The three roles the first version of that query was wrong about
//
// It asked has_table_privilege and nothing else, which on a correctly
// installed database returns five roles rather than two. Measured:
// collector and beacon_writer, as intended, plus postgres, plus
// schema_admin, plus pg_write_all_data. The last three hold INSERT
// through being a superuser, owning the table, and being one of
// PostgreSQL's predefined roles - none of which is a grant, and none of
// which describes a service.
//
// Left in, the defect would have been a rebuilt copy of the one this
// whole phase removes. schema_admin is a role a component connects as -
// upgrader.example.toml carries schema_admin_dsn - so the day anything
// running as the owner writes a heartbeat row, that row would be a
// silent address writer that can never hold a key, and full mode would
// be unreachable again, this time for a reason nobody could find.
//
//   - rolcanlogin, because a service connects. It also drops the
//     predefined roles for the right reason rather than by matching
//     their names, and it costs nothing when a grant arrives through a
//     group: has_table_privilege follows inheritance, so the login role
//     answers for itself.
//   - NOT rolsuper, because a superuser holds every privilege on every
//     table and so answers yes about tables it has never heard of.
//   - not the owner of either table, for the reason above. A collector
//     genuinely misconfigured to run as the owner is still counted -
//     through the first of the two ways a row qualifies, since its row
//     reports present or absent like any other collector's.
//
// The oid form of has_table_privilege, and the join to pg_roles, are
// both deliberate: the name form raises an error for a role that does
// not exist, so a heartbeat row outliving its role would fail the whole
// query rather than be skipped. to_regclass rather than a cast for the
// same shape of reason - on a database whose analytics schema has not
// been applied yet it yields NULL instead of failing, and a NULL table
// makes has_table_privilege NULL, so nobody qualifies, which refuses.
func Reports(ctx context.Context, pool *pgxpool.Pool) ([]Report, error) {
	if pool == nil {
		return nil, fmt.Errorf("tokenkey: no database")
	}
	// Read the rows through internal/heartbeat rather than with a query
	// here, so the accommodation for a database that has not applied the
	// column yet lives in one place. On such a database every service
	// comes back unknown, which refuses - and refusing is what the
	// unreachable field did too, now for a reason an operator can read.
	beats, err := heartbeat.Read(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("tokenkey: reading service heartbeats: %w", err)
	}
	if len(beats) == 0 {
		return nil, nil
	}

	writers, err := addressWriters(ctx, pool)
	if err != nil {
		return nil, err
	}

	out := make([]Report, 0, len(beats))
	for _, b := range beats {
		if b.IPTokenKey == heartbeat.TokenKeyUnknown && !writers[b.Service] {
			continue
		}
		out = append(out, Report{Service: b.Service, State: b.IPTokenKey})
	}
	// Ordered by service, so a sentence naming two of them reads the
	// same way twice. heartbeat.Read orders by beat time, which is right
	// for a health page and arbitrary for a message.
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// addressWriters is the set of roles a deployment granted the ability to
// write an address. See Reports for every clause.
func addressWriters(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		SELECT r.rolname
		  FROM pg_catalog.pg_roles r
		 WHERE r.rolcanlogin
		   AND NOT r.rolsuper
		   AND r.oid NOT IN (SELECT c.relowner
		                       FROM pg_catalog.pg_class c
		                      WHERE c.oid IN (to_regclass('traffic_snapshots'),
		                                      to_regclass('beacon_events')))
		   AND (has_table_privilege(r.oid, to_regclass('traffic_snapshots'), 'INSERT')
		     OR has_table_privilege(r.oid, to_regclass('beacon_events'), 'INSERT'))`)
	if err != nil {
		return nil, fmt.Errorf("tokenkey: asking which roles may write an address: %w", err)
	}
	defer rows.Close()

	writers := map[string]bool{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("tokenkey: scanning address writers: %w", err)
		}
		writers[role] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tokenkey: scanning address writers: %w", err)
	}
	return writers, nil
}

// Missing returns the writers that did not report a usable key.
//
// # The rule, and the case it is written around
//
// Every address writer that has reported must report a usable key, and
// at least one must have reported. Not "the collector and the beacon":
// a deployment with a collector and no beacon snippet is an ordinary,
// supported installation - the dashboard says so in its own comments -
// and demanding a key from a service that has never run would make full
// mode unreachable on every one of them. That is the defect this whole
// phase exists to remove, and rebuilding it inside the rule would be a
// poor joke.
//
// An empty reports list is therefore not "everybody is ready": the
// caller has to treat "nobody has spoken" as a refusal of its own, and
// both callers do.
func Missing(reports []Report) []Report {
	var out []Report
	for _, r := range reports {
		if !r.Ready() {
			out = append(out, r)
		}
	}
	return out
}
