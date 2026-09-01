// Package dblock holds the advisory lock keys that production code takes.
//
// A leaf on purpose: it imports nothing. The applier needs the schema
// key and so do the suites that apply a schema file by hand, and those
// two live on opposite sides of every import edge in this repository -
// internal/testdb cannot import internal/applier, because the applier's
// own tests are in package applier and import testdb. A constant with
// no dependencies is reachable from both without a cycle.
//
// internal/testdb keeps its own keys for locks that exist only to stop
// one suite's rows from landing in another's assertions. Those never
// appear in a deployment. The keys here do.
package dblock

// SchemaApply is held for the whole of one schema application.
//
// # What two schema applications at once do to each other
//
// Applying a schema file twice over is safe by design: every CREATE has
// an IF NOT EXISTS and every policy is dropped before it is created. But
// "safe twice in a row" and "safe twice at once" are different
// properties, and only the first had ever been tested.
//
// Measured, three sessions applying every file twelve times over with no
// lock_timeout set - 17 of 360 failed:
//
//	internal/retention/schema.sql     tuple concurrently updated (XX000)
//	internal/asnlookup/schema.sql     tuple concurrently updated (XX000)
//	internal/upgrade/schema.sql       deadlock detected          (40P01)
//	internal/rangerefresh/schema.sql  deadlock detected          (40P01)
//
// The causes are ordinary and cannot be fixed statement by statement.
// CREATE OR REPLACE FUNCTION rewrites its pg_proc row whether or not the
// body changed. DROP POLICY IF EXISTS followed by CREATE POLICY rewrites
// every policy on the table on every application - and that pair cannot
// become conditional, because dropping and recreating is exactly how an
// edited policy reaches an installed database. A guard that skipped it
// when the policy already existed would make every future policy change
// silently do nothing, which is a worse bug than this one.
//
// # What this lock is and is not for
//
// It is not what keeps the applier out of that window. The applier sets
// lock_timeout to 250ms and gives way on ordinary table locks long
// before it gets deep enough to collide in the catalogue: five runs of
// three concurrent appliers with this lock deliberately defeated
// produced no XX000 and no 40P01 at all.
//
// What it gives way *with* is the point. Measured, three appliers
// applying every file eight times each:
//
//	with this lock          24 applied,  0 gave way
//	with it defeated         8 applied, 16 gave way
//
// Two thirds of the work became requeues - each one a customer's upgrade
// waiting another tick for a reason nothing on their screen explains.
// Holding this instead means the second applier waits for the first and
// then does its work, which is what "one applier at a time" was always
// supposed to mean.
//
// The queue already tries to mean it: a partial unique index allows one
// in-flight request and an applier holds a claim while it works. That is
// not a guarantee - a claim goes stale after fifteen minutes and another
// applier takes it over, so an applier that is slow rather than dead is
// still inside the schema when its replacement starts.
//
// The other half is internal/testdb.SchemaApplyLock, which is this same
// key. A suite that applies a schema file by hand has no lock_timeout
// and does reach the collisions above - which is how a plain
// `go test -tags integration ./...` went red on its second run, in
// internal/asnlookup, reporting a grant that was never wrong. Sharing
// the key is what lets a test and a running applier serialise.
//
// Taken after the connection's lock_timeout is set, deliberately: a
// second applier that cannot have it within 250ms fails with 55P03
// rather than waiting, and 55P03 is already the path that requeues the
// request and retries on the next tick. Waiting indefinitely would hold
// a claim while doing nothing, which is how the stale claim that allows
// two appliers gets created in the first place.
const SchemaApply = 0x736368656d61 // "schema"
