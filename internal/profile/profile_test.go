package profile

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/memlimit"
)

// TestEveryProfileHasADistinctLevelAndMatchFindsIt.
//
// Match is the whole "derive the name, do not store it" claim, and it
// only works while the mapping is one-to-one. Two profiles sharing a
// level would make Match return whichever came first in the slice, and
// an installation would report a name it had not been given - silently,
// and differently depending on the order somebody happened to write them
// in.
func TestEveryProfileHasADistinctLevelAndMatchFindsIt(t *testing.T) {
	seenLevel := map[Level]string{}
	seenID := map[string]bool{}
	for _, p := range All() {
		if other, dup := seenLevel[p.Level]; dup {
			t.Errorf("%s and %s both claim level %q, so Match cannot tell them apart "+
				"and an installation would be told it is whichever is listed first",
				other, p.ID, p.Level)
		}
		seenLevel[p.Level] = p.ID

		if seenID[p.ID] {
			t.Errorf("two profiles share the id %q", p.ID)
		}
		seenID[p.ID] = true

		got, ok := Match(p.Level)
		if !ok || got.ID != p.ID {
			t.Errorf("Match(%q) = %+v (ok=%v), want %s", p.Level, got, ok, p.ID)
		}
		if byID, ok := ByID(p.ID); !ok || byID.Level != p.Level {
			t.Errorf("ByID(%q) = %+v (ok=%v), want level %q", p.ID, byID, ok, p.Level)
		}
	}

	if _, ok := Match(Level("bir-sey-degil")); ok {
		t.Error("Match accepted a level no profile has; an installation configured " +
			"by hand into a state none of these describe must come out as unmatched, " +
			"which is what lets the panel say Özel instead of guessing")
	}
}

// TestTheProfilesAreOrderedAndTheirBudgetsAgreeWithThatOrder.
//
// A list a person picks from, so the order is part of the interface -
// and a budget that disagreed with the order would be worse than no
// budget: somebody choosing the smaller-sounding option would get the
// larger one.
func TestTheProfilesAreOrderedAndTheirBudgetsAgreeWithThatOrder(t *testing.T) {
	ps := All()
	for i := 1; i < len(ps); i++ {
		prev, cur := ps[i-1], ps[i]
		if cur.Held < prev.Held {
			t.Errorf("%s is listed after %s and holds less (%d < %d)",
				cur.ID, prev.ID, cur.Held, prev.Held)
		}
		if cur.Peak < prev.Peak {
			t.Errorf("%s is listed after %s and peaks lower (%d < %d)",
				cur.ID, prev.ID, cur.Peak, prev.Peak)
		}
		if cur.Floor < prev.Floor {
			t.Errorf("%s is listed after %s and has a lower floor (%d < %d)",
				cur.ID, prev.ID, cur.Floor, prev.Floor)
		}
	}

	// And the peak is never below the held figure, which would mean a
	// table that costs less to build than to keep.
	for _, p := range ps {
		if p.Peak < p.Held {
			t.Errorf("%s peaks at %d and holds %d; a parse cannot cost less than "+
				"what it produces", p.ID, p.Peak, p.Held)
		}
	}
}

// TestRateStoreBytes.
//
// The formula is the interesting part, not the arithmetic: the map holds
// one entry per *admitted* connection's address, the limiter admits at
// most requestsPerSecond of them a second, and an entry lives ttlSeconds
// past its last request. So the ceiling is the product - a property of
// two settings rather than of how busy the site is.
func TestRateStoreBytes(t *testing.T) {
	// The shipped defaults: 500 requests a second, 300 second TTL.
	got, bounded := RateStoreBytes(500, 300)
	if !bounded {
		t.Fatal("the shipped defaults produced no bound")
	}
	if want := uint64(500 * 300 * bytesPerTrackedAddress); got != want {
		t.Errorf("RateStoreBytes(500, 300) = %d, want %d", got, want)
	}
	if mbs := got / mb; mbs < 15 || mbs > 40 {
		t.Errorf("the default rate store budget came out %d MB.\n"+
			"Measured, it should be about 23 - if this has moved a long way, either "+
			"the defaults changed or the per-address cost did, and the profile "+
			"budgets were computed against the old one", mbs)
	}

	// No limit is a real configuration and it has no ceiling. Reporting
	// zero-and-unbounded is the honest answer; returning a number would
	// hand the caller a bound that does not exist.
	for _, tc := range []struct{ rps, ttl int }{{0, 300}, {500, 0}, {-1, 300}} {
		if _, bounded := RateStoreBytes(tc.rps, tc.ttl); bounded {
			t.Errorf("RateStoreBytes(%d, %d) claimed a bound; with no limit there "+
				"is nothing bounding the map", tc.rps, tc.ttl)
		}
	}
}

// TestFits.
//
// The check the whole phase exists for: the customer is free to choose
// anything that will run, and stopped only from choosing what would be
// killed by the kernel.
func TestFits(t *testing.T) {
	rateStore, bounded := RateStoreBytes(500, 300)
	tam, _ := ByID("tam")
	dengeli, _ := ByID("dengeli")
	hafif, _ := ByID("hafif")

	cases := []struct {
		name    string
		profile Profile
		ceiling memlimit.Limit
		wantOK  bool
	}{
		{
			// Measured: the resolver alone survived 2 times in 5 at this
			// size. A coin toss is a refusal.
			name:    "the largest profile in a 256 MB container",
			profile: tam,
			ceiling: memlimit.Limit{Bytes: 256 * mb, From: memlimit.SourceCgroupV1},
			wantOK:  false,
		},
		{
			// 5/5 for the resolver alone - but the real collector also
			// carries the rate store and everything else, so this size
			// is still refused. The measurement is the floor, not the
			// budget.
			name:    "the largest profile in a 320 MB container",
			profile: tam,
			ceiling: memlimit.Limit{Bytes: 320 * mb, From: memlimit.SourceCgroupV1},
			wantOK:  false,
		},
		{
			name:    "the largest profile in a 512 MB container",
			profile: tam,
			ceiling: memlimit.Limit{Bytes: 512 * mb, From: memlimit.SourceCgroupV1},
			wantOK:  true,
		},
		{
			// Measured 1/5 at 128m and 5/5 at 160m for the resolver
			// alone; with the rest of the collector this size is refused.
			name:    "the middle profile in a 192 MB container",
			profile: dengeli,
			ceiling: memlimit.Limit{Bytes: 192 * mb, From: memlimit.SourceCgroupV1},
			wantOK:  false,
		},
		{
			name:    "the middle profile in a 256 MB container",
			profile: dengeli,
			ceiling: memlimit.Limit{Bytes: 256 * mb, From: memlimit.SourceCgroupV1},
			wantOK:  true,
		},
		{
			name:    "the smallest profile in a 128 MB container",
			profile: hafif,
			ceiling: memlimit.Limit{Bytes: 128 * mb, From: memlimit.SourceCgroupV2},
			wantOK:  true,
		},
		{
			name:    "an unknown ceiling permits anything",
			profile: tam,
			ceiling: memlimit.Limit{From: memlimit.SourceUnknown},
			wantOK:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := tc.profile.Fits(tc.ceiling, rateStore, bounded)
			if ok != tc.wantOK {
				t.Fatalf("Fits() = %v (%q), want %v", ok, why, tc.wantOK)
			}
			if ok {
				if why != "" {
					t.Errorf("a profile that fits came with an explanation: %q", why)
				}
				return
			}
			// A refusal has to say enough to act on: which profile, what
			// it wanted, what there is, and where that number came from.
			for _, want := range []string{tc.profile.Label, "MB", "konteyner bellek sınırı"} {
				if !strings.Contains(why, want) {
					t.Errorf("the refusal does not mention %q, so it does not say what "+
						"to change:\n%s", want, why)
				}
			}
		})
	}
}

// TestAnUnknownCeilingIsNotAnOutage.
//
// Its own test because it is the branch most likely to be "tightened"
// by somebody reading Fits and thinking a missing ceiling should be
// refused.
//
// It must not be. This check exists to keep a site up; a container with
// a masked /proc, or a kernel whose cgroup files have moved, would
// otherwise turn into a deployment that cannot select any profile at
// all - an outage caused by the thing meant to prevent one.
func TestAnUnknownCeilingIsNotAnOutage(t *testing.T) {
	for _, p := range All() {
		ok, why := p.Fits(memlimit.Limit{From: memlimit.SourceUnknown}, 0, false)
		if !ok {
			t.Errorf("%s was refused because the ceiling could not be read (%q).\n"+
				"An unreadable file must not become an outage", p.ID, why)
		}
	}
}

// TestTheRefusalNamesWhereTheCeilingCameFrom.
//
// A container limit and free memory on a shared machine are worth
// different amounts - the first is exact and this process's own, the
// second is an estimate the database next door can invalidate a minute
// later. An operator who is told which one they are looking at knows
// whether to raise a limit or to go and look at what else is running.
func TestTheRefusalNamesWhereTheCeilingCameFrom(t *testing.T) {
	tam, _ := ByID("tam")
	tiny := uint64(64 * mb)

	for _, tc := range []struct {
		from memlimit.Source
		want string
	}{
		{memlimit.SourceCgroupV1, "konteyner bellek sınırı"},
		{memlimit.SourceCgroupV2, "konteyner bellek sınırı"},
		{memlimit.SourceAvailable, "makinenin şu an boş belleği"},
	} {
		_, why := tam.Fits(memlimit.Limit{Bytes: tiny, From: tc.from}, 0, true)
		if !strings.Contains(why, tc.want) {
			t.Errorf("a ceiling from %q was explained as %q, wanted it to say %q",
				tc.from, why, tc.want)
		}
	}
}

// TestAnUnboundedRateStoreIsSaidOutLoud.
//
// With no request limit the map has no ceiling, so the budget is missing
// a term. Reporting the shortfall without saying so would give a precise
// number for an imprecise thing.
func TestAnUnboundedRateStoreIsSaidOutLoud(t *testing.T) {
	tam, _ := ByID("tam")
	_, why := tam.Fits(memlimit.Limit{Bytes: 64 * mb, From: memlimit.SourceCgroupV1}, 0, false)
	if !strings.Contains(why, "istek sınırı kapalı") {
		t.Errorf("the refusal quotes a budget computed without the rate store and "+
			"does not say the rate store is unbounded:\n%s", why)
	}
}
