// The role names, in a file with no build tag.
//
// Separated from testdb.go, which is behind `integration`, so that the
// check comparing them against the CI workflow runs in the unit gate.
// That check needs no database - it reads a YAML file and a slice - and
// the gate it belongs in is the cheapest one, because the failure it
// catches (a role the workflow forgot) surfaces as a SASL error inside
// the *slowest* job otherwise.
package testdb

// The five service roles, named so a suite says which one it means.
const (
	// Collector writes traffic_snapshots and refreshes the address
	// ranges.
	Collector = "collector"
	// Beacon writes beacon_events. It cannot read them back: the beacon
	// holds INSERT alone, which is why a suite that verifies what it
	// wrote needs Reader as well.
	Beacon = "beacon_writer"
	// Reader is the read-only API's role. SELECT on both analytics
	// tables and nothing else.
	Reader = "analytics_reader"
	// Panel is the panel's. Every panel_* table, and no access at all to
	// either analytics table - the isolation the product rests on.
	Panel = "panel_user"
	// SchemaAdmin is L3's applier: the one role in this deployment that
	// may run DDL. It owns the tables, so it can ALTER them, and holds
	// nothing outside this database - no CREATE ROLE, no other schema.
	//
	// The fifth role exists so that no running service has to carry a
	// superuser DSN on disk for its whole life.
	SchemaAdmin = "schema_admin"
)

// AllRoles is every role a suite connects as.
//
// Here so it can be compared against the places outside Go that also
// have to know: the CI workflow sets each role's password, and a list
// there that falls behind produces SASL failures rather than a message
// about a missing role.
//
// It fell behind exactly once, and it cost weeks of red runs: L3 added
// schema_admin, the workflow's `for role in ...` line kept naming four,
// and every applier and upgrade test failed to authenticate. See
// TestTheWorkflowKnowsEveryRole.
var AllRoles = []string{Collector, Beacon, Reader, Panel, SchemaAdmin}
