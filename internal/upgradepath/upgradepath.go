// Package upgradepath holds one question, asked against real databases:
// is a deployment that upgraded the same as one installed fresh?
//
// It has no production code. What lives here is the test that builds
// both kinds of database - one from this tree's schema plus its
// privilege matrix, one from the previous release's schema and then
// this tree's, the way cmd/upgrader does it - and compares them.
//
// # Why the question needed asking
//
// There are two paths to a database's shape and they run different
// files:
//
//	fresh install   release/install.sh: every schema file, then
//	                release/sql/grants.sql
//	upgrade         cmd/upgrader: the embedded schema files, and
//	                nothing else - the applier is the only component
//	                allowed to run DDL and it runs exactly what its
//	                fingerprint covers
//
// Nothing compared the results, so a table created in a schema file
// whose GRANT was written only in grants.sql came out of an upgrade
// with no role able to touch it. Measured against v0.23.0 before this
// package existed, three tables were in that state, two of them added
// in the unreleased window:
//
//	traffic_rollup, traffic_rollup_state  the collector could not
//	                                      write and the read API could
//	                                      not read - the summary path
//	                                      the dashboard's long ranges
//	                                      depend on
//	panel_member_invites                  panel_user held nothing, so
//	                                      inviting a member failed
//
// Each was correct on a fresh install and broken on every upgraded one,
// which is the half nobody was running.
//
// # Why a whole package
//
// It needs databases of its own - two of them - and a TestMain to build
// them, and internal/applier already has its own suite sharing the
// development database. The rule this repository learned the hard way
// is that a suite which changes another database's shape should not run
// in it.
package upgradepath
