// Package schemafiles is every schema.sql in this repository, in the
// order they must be applied.
//
// # Why the binary carries them
//
// L3's applier has to be able to migrate a database with nothing but
// itself: the release ships a schema directory, but a directory is
// something an operator can half-copy, mount at the wrong path, or
// leave from a previous version. A binary that verified a fingerprint
// and then applied a directory would be checking one thing and doing
// another.
//
// So the files are embedded, keyed by the same repository-relative path
// internal/schemaver hashes. The applier can therefore prove that what
// it is about to run is exactly what the request asked for, rather than
// assuming it.
//
// # The order is part of the schema
//
// Two of the entries below cannot move:
//
//   - retention is applied after storage and beacon, because its
//     functions read both hypertables and its grants name both writers.
//   - schemaver is applied last of all, because the row it writes
//     asserts that every file above it has already been applied. A
//     version recorded before the schemas it describes is a claim about
//     work that has not happened.
//
// release/build.sh numbers the files in this same order when it stages
// them, and TestTheOrderMatchesTheReleasePackage keeps the two from
// drifting - a second ordering nobody checks is the shape this project
// has found broken more than once.
package schemafiles

import (
	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/retention"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/storage"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// File is one schema file and where it lives.
type File struct {
	// Path is relative to the repository root, and is the key
	// schemaver.FingerprintOf hashes. A file that moves changes the
	// fingerprint, which is correct: moving a table between packages is
	// a schema change even when the SQL is byte-identical.
	Path string
	// SQL is the file's contents.
	SQL string
}

// InOrder is every schema file, in the order they must be applied.
//
// A slice rather than a map because the order is load-bearing and Go
// randomises map iteration - a schema applied in a different sequence on
// every run is the least debuggable failure this could have.
var InOrder = []File{
	{"internal/panel/schema.sql", panel.SchemaSQL},
	{"internal/storage/schema.sql", storage.SchemaSQL},
	{"internal/beacon/schema.sql", beacon.SchemaSQL},
	{"internal/asnlookup/schema.sql", asnlookup.SchemaSQL},
	{"internal/heartbeat/schema.sql", heartbeat.SchemaSQL},
	// After the two hypertables and both writers exist.
	{"internal/retention/schema.sql", retention.SchemaSQL},
	{"internal/logsink/schema.sql", logsink.SchemaSQL},
	{"internal/upgrade/schema.sql", upgrade.SchemaSQL},
	// The refresh queue, after asnlookup: its comments point at
	// ip_range_fetches and a reader applying these in order should meet
	// the table before the queue that fills it.
	{"internal/rangerefresh/schema.sql", rangerefresh.SchemaSQL},
	// The release update queue. After the schema queue it mirrors, so a
	// reader applying these in order meets the smaller version of the
	// same idea first.
	{"internal/relupdate/schema.sql", relupdate.SchemaSQL},
	// The backup queue and its catalogue. After the release queue,
	// because it is the same shape again and a reader meeting them in
	// order sees the pattern before the third copy of it - and because
	// its comments point at the isolation the earlier files set up.
	{"internal/backup/schema.sql", backup.SchemaSQL},
	// Last of all: what this records is "everything above was applied".
	{"internal/schemaver/schema.sql", schemaver.SchemaSQL},
}

// Map is the same set keyed by path, which is the shape
// schemaver.FingerprintOf takes.
func Map() map[string]string {
	out := make(map[string]string, len(InOrder))
	for _, f := range InOrder {
		out[f.Path] = f.SQL
	}
	return out
}

// Fingerprint is what this build's embedded schema hashes to.
//
// It should equal schemaver.Fingerprint, and TestTheEmbeddedSchemaIsThe
// OneTheConstantNames is what makes that a fact rather than a hope. The
// two are computed from different places on purpose: the constant is
// written by hand and this is derived from the bytes actually compiled
// in, so a schema edited without the constant moving is caught here as
// well as by the mirror test in schemaver.
func Fingerprint() string {
	return schemaver.FingerprintOf(Map())
}
