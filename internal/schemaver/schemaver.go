// Package schemaver answers one question: does the schema in this
// database match the one this binary was built against.
//
// # The measurement this package exists because of
//
// Measured 2026-08-30 against a real TimescaleDB, before any of this was
// written, because whether an upgrade needs a restart depended on it.
//
// Case A - schema newer than the binary, one column the binary does not
// know about:
//
//	written=1  failed=0  err=<nil>
//
// The old binary writes perfectly. CopyFrom names its columns, every
// ADD COLUMN in this repository carries a default, and nobody minds.
//
// Case B - binary newer than the schema, one column missing:
//
//	NewWriter succeeded — Ping passed, startup silent
//	first write: column "asn_org" does not exist (SQLSTATE 42703)
//	written=0  failed=3  rows actually in the table: 0 of 3
//
// The process comes up looking healthy and loses every row it is handed.
// Ping proves the database answers; it proves nothing about shape.
//
// Those two results are the whole design:
//
//   - Migrating the schema needs no restart. Case A says the running
//     binaries carry on through it.
//   - The order is schema first, binary second. The reverse is Case B.
//   - Startup must stop being silent. That is L2; this package is what
//     L2 and L3 both read.
//
// # Why a number and a fingerprint
//
// They answer different questions and neither can do the other's job:
//
//	Version      "is the binary ahead of the database?"  must be ordered
//	Fingerprint  "is what is installed really this?"     must not lie
//
// An integer is ordered but can lie - somebody edits a schema and
// forgets to bump it, or bumps it and forgets to edit. A hash cannot
// lie but has no order: given two different hashes, neither is newer.
//
// So both, and the thing that keeps the integer honest is the mirror
// test in this package: it recomputes the fingerprint from the schema
// files on disk and fails when it disagrees with the constant below.
// You cannot change a schema without meeting that test, and the test
// asks for the version bump.
//
// # What this package deliberately does not do
//
// It does not read the filesystem. FingerprintOf takes the file contents
// as an argument, so the constant and the recomputation are genuinely
// two sides of a mirror rather than one function called twice. A package
// that hashed its own schema files at init would agree with itself
// forever and prove nothing.
//
// It also runs no DDL. Nothing in this repository's service binaries
// does - see internal/storage/writer.go and internal/panel/store.go -
// and this package reading a version does not change that.
package schemaver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Version is the schema this build expects.
//
// Bumped by hand, and the mirror test is what makes the hand reliable:
// change any schema.sql and TestTheFingerprintMatchesTheSchemaOnDisk
// fails until this and Fingerprint are both updated.
//
// It starts at 1 rather than 0 because 0 is what an unset integer column
// reads as, and "never recorded" and "version zero" must not look alike.
//
// # Version 7 is a change of rule, not a change of shape
//
// It is the one bump in this file that no schema.sql caused. Two files
// had their prose corrected on 2026-09-02 and, under the old rule of
// hashing raw bytes, that moved Fingerprint - which State.Matches
// compares, which the health page and the upgrade button both read.
// A corrected sentence would have put "your schema does not match" and
// a developer-password-gated upgrade in front of every installation.
//
// So the rule changed instead: FingerprintOf now hashes the DDL, with
// comments stripped (see StripComments). Future comment fixes cost
// nothing. Adopting the rule costs one hash change, which every existing
// database sees once - and a hash that moves needs a number to move with
// it or the mismatch has no order, which is this bump.
//
// Version 8 is an ordinary one: service_heartbeat gained a profile
// column (A2). Additive, so an older binary writing into it carries on
// unaffected - Case A above - and the heartbeat reporter degrades to the
// column list it has rather than refusing, because monitoring is the one
// writer that must survive the window this number exists to describe.
//
// Version 9 adds panel_release_requests - the queue behind the panel's
// update button, V2 - and forces row-level security on the three request
// queues.
//
// The second half is a fix rather than a feature, and additive is the
// wrong word for it: every table is owned by schema_admin, and
// PostgreSQL exempts an owner from row-level security unless the table
// FORCEs it. So the "one role asks, another answers" split those three
// tables exist to enforce was, measured on a real database, not enforced
// against the answering role at all. An older binary is unaffected -
// nothing it does needs the permission that was removed - but a database
// that has not applied this is a database where the split is a comment.
//
// # 10
//
// One table: panel_release_available, the row the upgrader writes when
// it asks the release source which version is current.
//
// Additive, and the panel only reads it. The write policy names
// schema_admin alone, deliberately: the panel must not be able to tell
// itself that a version exists, because the reason the upgrader is the
// one asking is that the upgrader is the one holding the signing key.
const Version = 11

// Fingerprint is the SHA-256 of every schema.sql in this repository,
// canonically ordered. See FingerprintOf.
//
// Update it together with Version, never alone: a fingerprint that moved
// without the version moving is a schema change nobody can order.
const Fingerprint = "d70cf0e45d0aaba77161847506e26f24f03c0250a427da9d2305f318f1bc30cc"

// FingerprintOf hashes a set of schema files.
//
// The key is the path relative to the repository root, so a file that
// moves changes the fingerprint - moving a table between packages is a
// schema change, even when the SQL is byte-identical.
//
// Each file is reduced to its DDL first - see StripComments for why the
// prose is not part of the fact this value states.
//
// Sorted before hashing, because map iteration order is random in Go and
// a fingerprint that depended on it would differ between two runs over
// the same tree - the worst possible failure for a value whose whole job
// is to be comparable.
func FingerprintOf(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		ddl := StripComments(files[p])
		// Length-prefixed rather than separator-joined: two different
		// file sets can otherwise hash identically by moving bytes
		// across a separator, and a fingerprint with collisions anybody
		// can construct is not one to build a safety check on.
		fmt.Fprintf(h, "%d:%s\n%d:%s\n", len(p), p, len(ddl), ddl)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// State is what the database says about itself.
type State struct {
	// Recorded is false when the table exists but holds no row, and when
	// the table is not there at all. Both mean the same thing to a
	// reader - this database predates schema versioning - and they are
	// not worth two code paths.
	Recorded bool

	Version     int
	Fingerprint string
	AppliedBy   string
}

// Matches reports whether the database carries exactly what this build
// expects.
//
// Fingerprint, not version: the version is a label for people and the
// fingerprint is the fact. A database whose version says 1 and whose
// fingerprint is somebody else's schema is not a match, however
// reassuring the number looks.
func (s State) Matches() bool {
	return s.Recorded && s.Fingerprint == Fingerprint
}

// Ahead reports whether this build expects a newer schema than the one
// installed - the direction that loses data (Case B above).
//
// An unrecorded state counts as behind: a database that has never had a
// version written is, by construction, older than the first build that
// writes one.
func (s State) Ahead() bool {
	return !s.Recorded || s.Version < Version
}

// Behind reports whether the database is newer than this build. Safe
// (Case A) but worth saying out loud, because it means somebody
// downgraded a binary without downgrading a schema.
func (s State) Behind() bool {
	return s.Recorded && s.Version > Version
}

// ErrNoTable is returned when schema_version itself is absent, which is
// what every installation made before this package looks like.
var ErrNoTable = errors.New("schemaver: schema_version table is not there")

// Querier is the read this package needs, and nothing else. Narrow on
// purpose: a package that took a *pgxpool.Pool could grow a write.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Read asks the database what it is.
//
// A missing table is not an error the caller must handle specially - it
// is ErrNoTable, and the honest answer to "what version is this" for a
// database installed before versioning existed.
func Read(ctx context.Context, q Querier) (State, error) {
	var st State
	err := q.QueryRow(ctx, `
		SELECT version, fingerprint, applied_by
		FROM schema_version WHERE id = 1`).Scan(&st.Version, &st.Fingerprint, &st.AppliedBy)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Table there, row not. Treated as unrecorded rather than as an
		// error: it is the state a half-applied install leaves behind,
		// and the page that shows it should say "unknown", not "broken".
		return State{}, nil
	case err != nil:
		// 42P01 is undefined_table. Matched on the code rather than on
		// the message, which is localised and has changed between
		// PostgreSQL releases.
		if strings.Contains(err.Error(), "42P01") {
			return State{}, ErrNoTable
		}
		return State{}, fmt.Errorf("schemaver: read: %w", err)
	}

	st.Recorded = true
	return st, nil
}
