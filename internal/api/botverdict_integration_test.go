//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// botverdict_test.go holds the arithmetic half of this rule. This is the
// half that proves the product acts on it: a row whose only signal is a
// known-bot fingerprint has to come back counted as a bot, through the
// same query the panel's headline uses.
//
// The seeded score is not typed by hand. It comes from scoring.Score, so
// this test and the collector agree by construction rather than by two
// people having written the same number - the failure mode a fixture
// with hand-copied values has, and which cost this project a defect
// already (see internal/panel/analytics/technical.go).
func TestStore_RealTimescaleDB_AKnownBotFingerprintAloneCountsAsABot(t *testing.T) {
	const ja4 = "t13d3112h2_e8f1e7e78f70_b26ce05bbdd6"

	// 0,2 requests per second: the rate this deployment actually recorded
	// for the spoofing client that prompted this test. Well under the
	// rate component's saturation, so the fingerprint is doing all the
	// work - which is the case that used to be counted as a person.
	const politeRate = 0.2

	scored := scoring.Score(politeRate, ja4, scoring.KnownBots{ja4: "ua_spoof"}, 0, nil)
	if !scored.IsKnownBotJA4 {
		t.Fatalf("the probe fingerprint did not match its own known-bot set: %+v", scored)
	}
	if scored.RateScore >= DefaultBotScoreMin {
		t.Fatalf("the rate alone scores %d at %v rps, which already clears the cutoff %d - "+
			"this test would then pass without the fingerprint signal existing",
			scored.RateScore, politeRate, DefaultBotScoreMin)
	}

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	store := newTestStoreWithJA4(t, "t13d_api_spoof", []seedRow{{
		site: "site-spoof", ip: "198.51.100.0", at: base,
		rate: politeRate, score: int16(scored.Score), botJA4: scored.IsKnownBotJA4,
		ja4: ja4, currWin: 12,
	}})

	got, err := store.Summary(context.Background(), "site-spoof",
		base.Add(-time.Minute), base.Add(time.Minute), DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got.UniqueIPs != 1 {
		t.Fatalf("UniqueIPs = %d, want 1 (did the row land?)", got.UniqueIPs)
	}
	if got.BotIPs != 1 || got.HumanIPs != 0 {
		t.Errorf("an address whose only signal is a known-bot fingerprint came back as "+
			"bot_ips %d / human_ips %d, want 1 / 0 - it scored %d against a cutoff of %d",
			got.BotIPs, got.HumanIPs, scored.Score, DefaultBotScoreMin)
	}
}

// One address, two clients, and only one of them is a bot.
//
// This is the ordinary case rather than a corner: privacy.ip_storage =
// "masked" stores a /24, so an address in these tables is up to 256
// machines, and an office or a household is one address in either mode.
//
// The row must not mix them. Before representativeJA4 it did:
// bool_or(is_known_bot_ja4) and max(ja4) were independent aggregates, so
// the panel drew "score 50, known bot" beside whichever fingerprint
// sorted highest - on the live deployment, the one fingerprint in the
// group that was not in the known-bot set and therefore had no label.
// Nothing on the page could then say why the address had been scored.
//
// The fingerprints below are chosen so that plain max() would pick the
// wrong one: "t13d9..." sorts above "t13d1...", and it is the innocent
// one. A test whose fixture happened to sort the other way would pass
// against the defect.
func TestStore_RealTimescaleDB_TheReportedFingerprintIsTheOneThatWasFlagged(t *testing.T) {
	const (
		flagged  = "t13d1111h2_known_bot_here"
		innocent = "t13d9999h2_ordinary_browser"
	)
	if !(innocent > flagged) {
		t.Fatalf("the fixture no longer defeats max(): %q must sort above %q, "+
			"or this test passes whether the fix is there or not", innocent, flagged)
	}

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	store := newTestStoreWithJA4(t, "t13d_api_mixed", []seedRow{
		// The flagged one first, the innocent one later, so "most recent"
		// alone would also pick the wrong fingerprint.
		{site: "site-mixed", ip: "203.0.113.0", at: base,
			rate: 0.2, score: 50, botJA4: true, ja4: flagged, currWin: 6},
		{site: "site-mixed", ip: "203.0.113.0", at: base.Add(10 * time.Second),
			rate: 0.1, score: 0, botJA4: false, ja4: innocent, currWin: 3},
	})

	ctx := context.Background()
	from, to := base.Add(-time.Minute), base.Add(time.Minute)

	top, _, err := store.TopIPs(ctx, "site-mixed", from, to, 10, 0)
	if err != nil {
		t.Fatalf("TopIPs: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("TopIPs returned %d rows, want 1", len(top))
	}
	checkRepresentative(t, "TopIPs", top[0].IsKnownBotJA4, top[0].JA4, top[0].JA4Count, flagged)

	silent, _, err := store.SilentIPs(ctx, "site-mixed", from, to, 10, 0)
	if err != nil {
		t.Fatalf("SilentIPs: %v", err)
	}
	if len(silent) != 1 {
		t.Fatalf("SilentIPs returned %d rows, want 1", len(silent))
	}
	checkRepresentative(t, "SilentIPs", silent[0].IsKnownBotJA4, silent[0].JA4, silent[0].JA4Count, flagged)
}

func checkRepresentative(t *testing.T, who string, known bool, ja4 string, count int, want string) {
	t.Helper()
	if !known {
		t.Errorf("%s: is_known_bot_ja4 is false, but one of the address's rows was flagged", who)
	}
	if ja4 != want {
		t.Errorf("%s: reported fingerprint %q, want the flagged one %q - the flag and the "+
			"fingerprint beside it came from different rows", who, ja4, want)
	}
	if count != 2 {
		t.Errorf("%s: ja4_count = %d, want 2 - a page drawing one fingerprint has to be able "+
			"to say the address showed more", who, count)
	}
}
