//go:build integration

package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const seamSite = "p5-dikis"

// seedSeam writes one address to each table, in the modes the caller
// names.
//
// Its own inserter rather than seedTraffic/seedBeacon, and not because
// those are wrong: both call CleanSite, so the second call deletes the
// first's rows - which is a note in the project's own working file after
// it cost an afternoon. This writes both tables under one cleanup and
// controls the column the whole test is about.
//
// The address is the same in both tables. That matters: the seam is not
// two different visitors failing to join, it is *one* visitor that the
// join cannot recognise across a mode change, and a fixture using two
// addresses would show nothing.
func seedSeam(t *testing.T, when time.Time, collectorToken, beaconToken []byte) {
	t.Helper()

	admin := testdb.Admin(t)
	testdb.CleanSite(t, admin, seamSite)

	const ip = "203.0.113.77"

	collector := testdb.Pool(t, testdb.Collector)
	if _, err := collector.Exec(context.Background(), `
		INSERT INTO traffic_snapshots
		  (time, site_id, ip, ip_hash, ja4, prev_window_count, curr_window_count,
		   request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org,
		   is_known_bot_asn)
		VALUES ($1, $2, $3, $4, 't13d_p5_dikis', 1, 1, 1.0, 5, false, 'TR', 9121,
		        'Turk Telekom', false)`,
		when, seamSite, ip, nullIfEmpty(collectorToken),
	); err != nil {
		t.Fatalf("seeding the collector's row: %v", err)
	}

	beacon := testdb.Pool(t, testdb.Beacon)
	if _, err := beacon.Exec(context.Background(), `
		INSERT INTO beacon_events
		  (time, site_id, visitor_id, event_type, event_name, path, ip, ip_hash,
		   device, country, is_bot_ua)
		VALUES ($1, $2, 'v1', 'pageview', '', '/', $3, $4, 'desktop', 'TR', false)`,
		when, seamSite, ip, nullIfEmpty(beaconToken),
	); err != nil {
		t.Fatalf("seeding the beacon's row: %v", err)
	}
}

// nullIfEmpty turns an absent token into a real SQL NULL, because a
// zero-length bytea is not one - and the join keys on the token whenever
// it is not NULL, so an empty one would become a key every row shared.
func nullIfEmpty(token []byte) any {
	if len(token) == 0 {
		return nil
	}
	return token
}

// The page says when a window's addresses are not all keyed the same
// way, and says nothing when they are.
//
// # What this is for (PLAN.md §P5, the warning half)
//
// privacy.ip_storage changing does not rewrite history - that was the
// owner's decision and it stands. What it does is put a seam in the
// crossover join for as long as retention keeps rows from both sides of
// it: a token and a masked network never compare equal, so coverage
// reads lower than it was and nothing said so. The measurement of the
// seam itself is in internal/api; this is about whether a customer
// reading the page finds out.
//
// # The three states, and why all three
//
// A notice that is always shown is a notice nobody reads, so the silent
// case is as much of an assertion as the loud one. And the two loud
// cases say different things to do: a seam in time ends by itself when
// retention passes it, while two writers in different modes right now is
// a fault to go and fix.
func TestThePageSaysWhenAWindowSpansAModeChange(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)

	lang := srv.Renderer.Catalogs().Base()
	seam := shown(lang.T("pano.kesisim.dikis"))
	disagree := shown(lang.T("pano.kesisim.kip_ayrisiyor"))

	// One server, several accounts is the rule here: setupTestServer
	// holds testdb.AccountsLock until the test ends, so a second call
	// would wait for itself.
	client, base := developerOwner(t, srv, store, seamSite, "p5-dikis-sahibi")

	page := func() string {
		t.Helper()
		status, body := get(t, client, base+sitePath(seamSite))
		if status != http.StatusOK {
			t.Fatalf("the dashboard answered %d", status)
		}
		if strings.Contains(body, shown(lang.T("pano.bos.ulasilamiyor.trafik"))) {
			t.Fatal("the crossover section reports the API unreachable, so nothing " +
				"below is a measurement of this page")
		}
		return body
	}

	token := []byte("0123456789abcdef")
	when := time.Now().Add(-3 * time.Hour)

	// ---- consistent: no notice ----
	//
	// Both modes, because a rule that only looked for a token would be
	// silent in masked mode by accident rather than on purpose.
	for _, tc := range []struct {
		name  string
		token []byte
	}{
		{"masked on both sides", nil},
		{"full on both sides", token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedSeam(t, when, tc.token, tc.token)
			body := page()
			if strings.Contains(body, seam) {
				t.Error("the page warns about a mode change in a window that has one mode")
			}
			if strings.Contains(body, disagree) {
				t.Error("the page says the two writers disagree while they agree")
			}
			// The section did draw - otherwise the two assertions above
			// are satisfied by a page with no crossover section at all,
			// which is the shape of a test that measures nothing.
			if !strings.Contains(body, shown(lang.T("pano.kesisim.baslik"))) {
				t.Fatal("the crossover section is not on the page")
			}
		})
	}

	// ---- the two writers in different modes: the live fault ----
	t.Run("the collector tokenises and the beacon does not", func(t *testing.T) {
		seedSeam(t, when, token, nil)
		body := page()
		if !strings.Contains(body, disagree) {
			t.Error("the two sources are in different modes and the page does not say so.\n" +
				"This is the state in which coverage reads 0% and every beacon " +
				"address is reported as one the collector never saw - and the " +
				"explanation printed beside that number blames the network.")
		}
		if strings.Contains(body, seam) {
			t.Error("the page also claims a mode change inside the window; the two " +
				"notices say different things to do and only one can be true")
		}
	})

	// ---- a seam inside the window ----
	//
	// Both kinds on the collector's side: the mode changed while these
	// rows were being written, which is what a customer's window looks
	// like on the day they change the setting.
	t.Run("the collector holds both kinds", func(t *testing.T) {
		seedSeam(t, when, token, token)
		collector := testdb.Pool(t, testdb.Collector)
		if _, err := collector.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ip_hash, ja4, prev_window_count, curr_window_count,
			   request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org,
			   is_known_bot_asn)
			VALUES ($1, $2, '203.0.113.78', NULL, 't13d_p5_dikis', 1, 1, 1.0, 5,
			        false, 'TR', 9121, 'Turk Telekom', false)`,
			when.Add(time.Minute), seamSite,
		); err != nil {
			t.Fatalf("seeding the older-mode row: %v", err)
		}

		body := page()
		if !strings.Contains(body, seam) {
			t.Fatal("the window holds addresses keyed both ways and the page says nothing")
		}

		// Above the numbers, not below them. The notice says the
		// coverage underneath is lower than the site's really was, and a
		// caveat printed after a number is a caveat most readers never
		// reach - so the order is part of the claim rather than styling.
		notice, numbers := strings.Index(body, seam), strings.Index(body, `class="kesisim-ozet"`)
		if numbers < 0 {
			t.Fatal("the crossover summary list is not on the page")
		}
		if notice > numbers {
			t.Errorf("the notice is drawn at byte %d and the numbers at %d, so it "+
				"reads as a footnote to figures somebody has already believed",
				notice, numbers)
		}
	})

	// ---- and it withdraws itself ----
	//
	// The reason the condition is the data rather than a record of the
	// change: retention drops the older side, and the notice stops being
	// true without anything having to remember to stop saying it. Here
	// the deletion stands in for retention, which is the only difference
	// between this and waiting ninety days.
	t.Run("retention passing the seam ends it", func(t *testing.T) {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM traffic_snapshots WHERE site_id = $1 AND ip_hash IS NULL`,
			seamSite); err != nil {
			t.Fatalf("removing the older side of the seam: %v", err)
		}
		if strings.Contains(page(), seam) {
			t.Error("the older side of the seam is gone and the page still warns. " +
				"Nothing withdraws this notice but the data, so a notice that " +
				"outlives its cause outlives every cause.")
		}
	})
}
