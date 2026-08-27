//go:build integration

// Package testdb hands an integration suite the connection its
// production code actually uses.
//
// # Why it exists
//
// Every integration suite in this project used to open one pool, as
// `collector`, against a development database that `collector` had
// created. That role therefore owned every table, and PostgreSQL grants
// an owner everything - so ten suites ran with authority no deployment
// gives any service, while describing themselves as tests of a
// role-separated design.
//
// What that hid, found in one afternoon by installing the package and
// running it:
//
//   - The retention feature had never worked on an installed
//     deployment. TimescaleDB checks hypertable *ownership*, and no
//     service owns one. Both tables grew forever.
//   - ip_asn_ranges and ip_country_ranges had no grants at all, so ASN
//     and country enrichment was silently off everywhere.
//   - The setup wizard reported the analytics tables as missing on
//     every correct install, because information_schema hides tables
//     the current role has no privilege on - and a failed required
//     check blocks handover, so the deployment could never be given to
//     its owner.
//
// None of the three is subtle. All three were invisible from a suite
// whose fixture was more privileged than production.
//
// # The rule
//
// A test uses the role its code runs as. Setting up and tearing down
// are a different job - they are what the installer and the schema
// owner do - and they get Admin, which skips rather than pretends when
// no superuser connection is offered.
package testdb

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The four service roles, named so a suite says which one it means.
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
)

// DSN is where role connects.
//
// Overridable per role through the environment, because the passwords
// below are the development cluster's convention and nothing else.
func DSN(role string) string {
	if dsn := os.Getenv("CA_DSN_" + role); dsn != "" {
		return dsn
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:5432/analytics", role, role)
}

// Pool opens a connection as one service role, closed when the test ends.
func Pool(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), DSN(role))
	if err != nil {
		t.Fatalf("pgxpool.New as %s: %v", role, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as %s: %v\n"+
			"Is the database up and installed? release/install.sh creates the four roles "+
			"and applies the grants; the suites expect each role's password to be its own name.",
			role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Admin is a connection that owns the schema.
//
// For the things no service may do and none should be able to: DDL, and
// removing rows from the analytics tables. Neither writer holds DELETE -
// see internal/retention/schema.sql for why that is deliberate - so a
// suite that seeds rows needs this to clear them again.
//
// Skips when unset rather than failing. A machine that can run a suite
// as its service roles may have no superuser connection to offer, and a
// test that cannot run is not a test that failed.
func Admin(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN to a connection that owns the schema; this test writes rows only its owner can remove")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New (superuser): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// CleanSite removes one site's analytics rows, now and when the test
// ends.
//
// Both ends, because a suite that only cleans up afterwards inherits
// whatever a crashed previous run left behind - and a row from last time
// inside this run's window is a number nobody can explain.
func CleanSite(t *testing.T, admin *pgxpool.Pool, sites ...string) {
	t.Helper()
	wipe := func() {
		for _, table := range []string{"traffic_snapshots", "beacon_events"} {
			if _, err := admin.Exec(context.Background(),
				"DELETE FROM "+table+" WHERE site_id = ANY($1)", sites); err != nil {
				t.Logf("clearing %s for %v: %v", table, sites, err)
			}
		}
	}
	wipe()
	t.Cleanup(wipe)
}
