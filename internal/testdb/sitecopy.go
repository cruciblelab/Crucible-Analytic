//go:build integration

package testdb

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SiteCopy is a private panel_site_members and panel_settings for one
// suite: a deployment whose every site the suite chose.
//
// # Why a copy rather than the real tables
//
// H10 made a deployment-wide setting answer to "does this account own
// every site the deployment knows" - and the shared database knows every
// site any suite ever made, with more arriving from packages running in
// parallel. A one-customer deployment, where one owner owns everything,
// cannot be built there: the next suite's site makes it two customers
// mid-test, and the failure would surface as a refusal the product was
// right to give.
//
// So the two tables the question reads are copied into a schema of the
// caller's naming, first on the search path. Everything else - accounts,
// sessions, the audit log - stays shared, as it is for every other suite.
// HeartbeatCopy is the same shape for the same reason.
//
// The copies keep columns, defaults, constraints and indexes (LIKE ...
// INCLUDING ALL), so "an ownership may never be temporary" holds here as
// it does there. They do not keep foreign keys or row-level security:
// a membership may name a user id no account has, which is what lets the
// question be asked of rows alone.
type SiteCopy struct {
	t      *testing.T
	admin  *pgxpool.Pool
	schema string
}

// NewSiteCopy builds the schema and the two tables, empty.
//
// Call it before the pool that reads it, so its cleanup runs afterwards
// (see NewHeartbeatCopy).
func NewSiteCopy(t *testing.T, schema string) *SiteCopy {
	t.Helper()
	ctx := context.Background()
	admin := Admin(t)

	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
		`CREATE TABLE ` + schema + `.panel_site_members (LIKE public.panel_site_members INCLUDING ALL)`,
		`CREATE TABLE ` + schema + `.panel_settings (LIKE public.panel_settings INCLUDING ALL)`,
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + Panel,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.panel_site_members, ` +
			schema + `.panel_settings TO ` + Panel,
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
	return &SiteCopy{t: t, admin: admin, schema: schema}
}

// DSN is a connection string that reads and writes the copies. The base
// must have no query string of its own.
func (c *SiteCopy) DSN(base string) string {
	return base + "?options=-csearch_path%3D" + c.schema + ",public"
}

// Reaches fails the test unless a pool resolves both tables to the
// copies - a fixture the code does not read is testing the code's other
// input.
func (c *SiteCopy) Reaches(pool *pgxpool.Pool) {
	c.t.Helper()
	for _, table := range []string{"panel_site_members", "panel_settings"} {
		var where string
		if err := pool.QueryRow(context.Background(),
			`SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.oid = $1::regclass`, table).Scan(&where); err != nil {
			c.t.Fatalf("resolving %s: %v", table, err)
		}
		if where != c.schema {
			c.t.Fatalf("this pool resolves %s to %q, not to the fixture copy in %q",
				table, where, c.schema)
		}
	}
}

// Member records a membership. A zero expires is one that does not end.
func (c *SiteCopy) Member(site string, userID int64, role string, expires time.Time) {
	c.t.Helper()
	var end *time.Time
	if !expires.IsZero() {
		end = &expires
	}
	if _, err := c.admin.Exec(context.Background(), `
		INSERT INTO `+c.schema+`.panel_site_members (site_id, user_id, role, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role, expires_at = EXCLUDED.expires_at`,
		site, userID, role, end); err != nil {
		c.t.Fatalf("recording %s on %s as %s: %v", site, role, site, err)
	}
}

// Drop removes one membership.
func (c *SiteCopy) Drop(site string, userID int64) {
	c.t.Helper()
	if _, err := c.admin.Exec(context.Background(),
		`DELETE FROM `+c.schema+`.panel_site_members WHERE site_id = $1 AND user_id = $2`,
		site, userID); err != nil {
		c.t.Fatalf("dropping %d on %s: %v", userID, site, err)
	}
}

// Allow writes the beacon's allowlist (beacon.sites); no sites removes
// the row, so the setting is unset.
func (c *SiteCopy) Allow(sites ...string) {
	c.t.Helper()
	ctx := context.Background()
	if len(sites) == 0 {
		if _, err := c.admin.Exec(ctx, `DELETE FROM `+c.schema+`.panel_settings
			WHERE scope = 'global' AND site_id = '' AND key = 'beacon.sites'`); err != nil {
			c.t.Fatalf("clearing beacon.sites: %v", err)
		}
		return
	}
	value, err := json.Marshal(sites)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.admin.Exec(ctx, `
		INSERT INTO `+c.schema+`.panel_settings (scope, site_id, key, value)
		VALUES ('global', '', 'beacon.sites', $1::jsonb)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		string(value)); err != nil {
		c.t.Fatalf("writing beacon.sites: %v", err)
	}
}
