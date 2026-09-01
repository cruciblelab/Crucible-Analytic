//go:build integration

package testdb

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// A privilege nobody needs is a privilege nobody audits.
//
// That is H5's finding, and this is the same shape one level down: not a
// default PostgreSQL switched on without being asked, but a GRANT this
// project wrote on purpose for a permission the database never checks.
//
// # What was found
//
// Three grants of the form
//
//	GRANT USAGE, SELECT ON SEQUENCE <table>_id_seq TO <role>;
//
// against columns declared GENERATED ALWAYS AS IDENTITY. PostgreSQL
// treats an identity sequence as part of its column: INSERT on the table
// is the whole permission, and the sequence grant is checked by nothing.
//
// Measured rather than read off the manual - with every privilege on
// panel_upgrade_requests_id_seq revoked, panel_user inserted a row and
// got back id 740.
//
// The seven BIGSERIAL sequences are different and genuinely do need
// theirs, which is exactly why the two kinds get confused: they look
// identical in grants.sql and only the column declaration tells them
// apart.

// TestNoIdentitySequenceIsGranted.
//
// Reads the catalogue rather than grants.sql, so it is a fact about the
// database this deployment actually has rather than about a file
// somebody may have edited without applying.
//
// The identity sequences are found by asking which sequences are owned
// by an identity column - `pg_depend` with `deptype = 'i'` - rather than
// by matching names, because a name-based rule would be a hand list of
// exactly the kind this project keeps finding short.
func TestNoIdentitySequenceIsGranted(t *testing.T) {
	pool := Admin(t)
	ctx := context.Background()

	// aclexplode rather than information_schema.usage_privileges, and
	// the difference is not cosmetic: that view reports USAGE only, so a
	// GRANT SELECT on a sequence - which is half of what the removed
	// grants said - would have been invisible to a check written to find
	// them. Measured: the first version of this query saw five of the
	// grants and missed their SELECT halves entirely.
	rows, err := pool.Query(ctx, `
		SELECT s.relname, acl.grantee::regrole::text, acl.privilege_type
		FROM pg_class s
		JOIN pg_depend d
		  ON d.objid = s.oid AND d.classid = 'pg_class'::regclass AND d.deptype = 'i'
		JOIN pg_attribute a
		  ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		     AND a.attidentity IN ('a', 'd')
		CROSS JOIN LATERAL aclexplode(s.relacl) AS acl
		WHERE s.relkind = 'S'
		  AND acl.grantee <> 0
		  AND acl.grantee <> s.relowner
		ORDER BY s.relname, 2, 3`)
	if err != nil {
		t.Fatalf("reading sequence privileges: %v", err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var seq, grantee, priv string
		if err := rows.Scan(&seq, &grantee, &priv); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		found = append(found, seq+" -> "+grantee+" ("+priv+")")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)

	if len(found) > 0 {
		t.Errorf("%d privilege(s) granted on identity sequences, and the database "+
			"checks none of them:\n  %s\n"+
			"An identity sequence belongs to its column - INSERT on the table is the "+
			"whole permission, measured. Remove the GRANT from release/sql/grants.sql. "+
			"BIGSERIAL sequences are different and do need theirs; the column "+
			"declaration is what tells the two apart.",
			len(found), strings.Join(found, "\n  "))
	}
}

// TestTheIdentitySequenceScanSeesSomething.
//
// The half that stops the check above from passing by finding nothing.
//
// A query that returns no rows is indistinguishable from a query that is
// wrong, and this one is three joins deep into the catalogue - exactly
// the kind that keeps working after it has stopped meaning anything.
func TestTheIdentitySequenceScanSeesSomething(t *testing.T) {
	pool := Admin(t)

	var identitySequences int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM pg_class s
		JOIN pg_depend d
		  ON d.objid = s.oid AND d.classid = 'pg_class'::regclass AND d.deptype = 'i'
		JOIN pg_attribute a
		  ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		     AND a.attidentity IN ('a', 'd')
		WHERE s.relkind = 'S'`).Scan(&identitySequences); err != nil {
		t.Fatalf("counting identity sequences: %v", err)
	}

	// panel_logs, panel_upgrade_requests and ip_range_fetches, today.
	if identitySequences < 3 {
		t.Errorf("the catalogue query found %d identity sequences and this schema has "+
			"at least three (panel_logs, panel_upgrade_requests, ip_range_fetches).\n"+
			"So the check above is passing by looking at nothing, which is how it "+
			"would look on the day the query stopped matching PostgreSQL's catalogue",
			identitySequences)
	}
}
