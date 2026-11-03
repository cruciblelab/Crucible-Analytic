//go:build integration

package api

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// blocklistSchema holds this suite's own traffic_snapshots.
//
// The preview counts every site's last day, so on the shared table its
// totals would include whatever another package wrote a moment ago. A
// copy first on the search path (testdb.SiteCopy's shape) makes the
// whole table this test's.
const blocklistSchema = "ca_b3f_onizleme"

// TestTheBlocklistPreviewCountsEachRulesAddressesInTheLastDay.
//
// Built so each clause has a row that would move a number if it were
// gone:
//   - two snapshots of one address count it once;
//   - an address seen on two sites is one address (the blocklist is not
//     per site);
//   - full-mode rows are keyed by their token, so two tokens behind one
//     masked network are two addresses;
//   - a row 25 hours old counts for nothing, one 23h50m old does;
//   - an address the lookup did not place is in the total and in no rule;
//   - a rule nothing matched is answered with zero, in the order asked.
func TestTheBlocklistPreviewCountsEachRulesAddressesInTheLastDay(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS ` + blocklistSchema + ` CASCADE`,
		`CREATE SCHEMA ` + blocklistSchema,
		`CREATE TABLE ` + blocklistSchema + `.traffic_snapshots (LIKE public.traffic_snapshots INCLUDING ALL)`,
		`GRANT USAGE ON SCHEMA ` + blocklistSchema + ` TO ` + testdb.Reader,
		`GRANT SELECT ON ` + blocklistSchema + `.traffic_snapshots TO ` + testdb.Reader,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+blocklistSchema+` CASCADE`)
	})

	store, err := NewStore(ctx, testdb.DSN(testdb.Reader)+"?options=-csearch_path%3D"+blocklistSchema+",public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	var where string
	if err := store.pool.QueryRow(ctx, `SELECT n.nspname FROM pg_class c JOIN pg_namespace n
		ON n.oid = c.relnamespace WHERE c.oid = 'traffic_snapshots'::regclass`).Scan(&where); err != nil || where != blocklistSchema {
		t.Fatalf("the store reads traffic_snapshots from %q (%v), not the copy", where, err)
	}

	for _, r := range []struct {
		ago     string
		site    string
		ip      string
		hash    string
		country string
		asn     int
	}{
		{"1 hour", "bl-a", "10.0.1.0", "", "RU", 100},
		{"2 hours", "bl-a", "10.0.1.0", "", "RU", 100},
		{"3 hours", "bl-b", "10.0.1.0", "", "RU", 100},
		{"1 hour", "bl-a", "10.0.2.0", "", "RU", 200},
		{"1 hour", "bl-b", "10.0.3.0", "", "", 0},
		{"1 hour", "bl-a", "10.0.4.0", "x", "DE", 100},
		{"1 hour", "bl-a", "10.0.4.0", "y", "DE", 100},
		{"23 hours 50 minutes", "bl-b", "10.0.6.0", "", "NL", 0},
		{"25 hours", "bl-a", "10.0.5.0", "", "RU", 100},
	} {
		var hash any
		if r.hash != "" {
			hash = []byte(r.hash)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO `+blocklistSchema+`.traffic_snapshots
			    (time, site_id, ip, ip_hash, ja4, prev_window_count, curr_window_count,
			     request_rate, bot_score, country, asn)
			VALUES (now() - $1::interval, $2, $3::inet, $4, '', 0, 1, 0, 0, $5, $6)`,
			r.ago, r.site, r.ip, hash, r.country, r.asn); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.BlocklistPreview(ctx, []string{"FR", "RU", "DE"}, []int{300, 100, 200})
	if err != nil {
		t.Fatal(err)
	}
	if got.Addresses != 6 || got.WithCountry != 5 || got.WithASN != 4 {
		t.Errorf("addresses %d, with a country %d, with an ASN %d; want 6, 5, 4",
			got.Addresses, got.WithCountry, got.WithASN)
	}
	if want := []CountryHits{{"FR", 0}, {"RU", 2}, {"DE", 2}}; !reflect.DeepEqual(got.Countries, want) {
		t.Errorf("countries %+v, want %+v", got.Countries, want)
	}
	if want := []ASNHits{{300, 0}, {100, 3}, {200, 1}}; !reflect.DeepEqual(got.ASNs, want) {
		t.Errorf("asns %+v, want %+v", got.ASNs, want)
	}
	if d := got.To.Sub(got.From); d != BlocklistPreviewWindow {
		t.Errorf("the window is %s, want %s", d, BlocklistPreviewWindow)
	}
	if skew := time.Since(got.To); skew < -time.Minute || skew > time.Minute {
		t.Errorf("the window ends %s from now; it should be the database's now", skew)
	}

	// No rule at all: the totals alone, and empty lists rather than null.
	none, err := store.BlocklistPreview(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if none.Addresses != 6 || none.Countries == nil || none.ASNs == nil || len(none.Countries)+len(none.ASNs) != 0 {
		t.Errorf("with no rules: %+v", none)
	}
}
