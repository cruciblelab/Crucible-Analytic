package schemaver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RequireColumns refuses to let a writer start against a table that does
// not have the columns it names.
//
// # The measurement
//
// Against a real TimescaleDB, with one column dropped from the table a
// writer names:
//
//	NewWriter succeeded — Ping passed, startup silent
//	first write: column "asn_org" does not exist (SQLSTATE 42703)
//	written=0  failed=3  rows actually in the table: 0 of 3
//
// The process starts, reports healthy, and loses every row it is handed.
// Ping proves the database answers a question; it proves nothing about
// what shape the answer has.
//
// So the shape is asked directly, once, at startup - which turns a
// silent loss into a service that refuses to run and says which column.
// A collector that will not start is an outage somebody fixes in
// minutes. A collector that starts and drops rows is an outage nobody
// sees until the numbers are asked for, by which time the traffic is
// gone and there is nothing to recover.
//
// # Missing is fatal, extra is fine, and that asymmetry is the product
//
// A column in the table that the writer does not name is *not* an error,
// and it must never become one. That is the state a correct upgrade
// passes through: the schema goes first, the binaries follow, and in
// between every running binary is looking at a table with columns it has
// never heard of. Measured, it writes perfectly - CopyFrom names its
// columns and every ADD COLUMN in this repository carries a default.
//
// Making extra columns fatal would turn the safe half of an upgrade into
// an outage, and would force the order that loses data.
func RequireColumns(ctx context.Context, q Querier, table string, want []string) error {
	have, err := columnsOf(ctx, q, table)
	if err != nil {
		return err
	}
	if len(have) == 0 {
		return fmt.Errorf("schemaver: table %q is not there, or has no columns this role may see; "+
			"apply the schema before starting", table)
	}

	var missing []string
	for _, c := range want {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)

	// The message names the columns and the fix, because whoever reads
	// it is looking at a service that will not start and has to decide
	// what to do in the next minute.
	return fmt.Errorf(
		"schemaver: %s is missing %d column(s) this build writes: %s.\n"+
			"This build expects schema version %d. Upgrade the schema before starting this binary - "+
			"the schema goes first and the binaries follow, never the other way round. "+
			"Refusing to start rather than writing rows that would be dropped.",
		table, len(missing), strings.Join(missing, ", "), Version)
}

// columnsOf reads a table's columns from pg_catalog.
//
// pg_catalog rather than information_schema, and the difference has
// bitten this project before: information_schema filters by what the
// current user has privileges on, so a role that may write a table but
// holds no column privileges sees an empty list and concludes the table
// is missing. pg_catalog answers what is there.
func columnsOf(ctx context.Context, q Querier, table string) (map[string]bool, error) {
	rows, err := queryRows(ctx, q, `
		SELECT a.attname
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = $1
		  AND n.nspname = ANY (current_schemas(false))
		  AND a.attnum > 0
		  AND NOT a.attisdropped`, table)
	if err != nil {
		return nil, fmt.Errorf("schemaver: read columns of %s: %w", table, err)
	}
	return rows, nil
}

// Rows is the second half of what this package needs from a pool, kept
// separate from Querier so a caller that only reads the version does not
// have to supply it.
type Rows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func queryRows(ctx context.Context, q Querier, sql string, args ...any) (map[string]bool, error) {
	r, ok := q.(Rows)
	if !ok {
		return nil, fmt.Errorf("schemaver: this connection cannot run a multi-row query")
	}
	rows, err := r.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// HasColumn reports whether one column is there.
//
// RequireColumns' question is "may this build write at all", and its
// answer is a refusal. This is the other question, and it has a different
// answer: a build that can write most of a row and one column fewer.
//
// There is exactly one place that shape is right, and it is monitoring.
// The heartbeat is what tells an operator whether a service is alive, so
// it is the one writer that must keep working while the schema is
// mid-upgrade - the window between "new binary deployed" and "schema
// applied" is precisely when somebody is watching that page, and a
// heartbeat that refused to write during it would report every service
// as down at the moment of maximum anxiety.
//
// Everywhere else the refusal is right, because writing a row with a
// column missing loses data silently. Nothing about a heartbeat row is
// data: it is a status, replaced every minute.
//
// An error is not a "no". A role that cannot read pg_catalog, or a
// database that has gone away, is a different fact from a column that is
// absent, and the caller has to be able to tell them apart.
func HasColumn(ctx context.Context, q Querier, table, column string) (bool, error) {
	have, err := columnsOf(ctx, q, table)
	if err != nil {
		return false, err
	}
	return have[column], nil
}
