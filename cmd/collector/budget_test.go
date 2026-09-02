package main

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/memlimit"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
)

const mb = 1 << 20

func profileByID(t *testing.T, id string) profile.Profile {
	t.Helper()
	p, ok := profile.ByID(id)
	if !ok {
		t.Fatalf("no profile %q; the offered set changed and this test names one that "+
			"is gone", id)
	}
	return p
}

// TestBudgetVerdict.
//
// # The two rows that carry the decision
//
// "does not fit, enforced" and "does not fit, estimated" are the same
// arithmetic and opposite answers, and that is the whole design. A
// cgroup limit will kill this process for exceeding it; MemAvailable is
// what the machine happened to have free at one instant, on a box whose
// database is sized to take most of it.
//
// Getting them the same way round would be wrong in both directions. Refusing
// on an estimate takes a site down for a busy minute. Starting under an
// enforced limit that cannot hold the datasets only chooses when the
// site goes down instead of whether.
//
// The sizes are the measured ones: Tam needs its 320 MB floor plus the
// rate store plus the 48 MB allowance, which is why 256 MB is short and
// 512 MB is not.
func TestBudgetVerdict(t *testing.T) {
	const noRateStore = 0

	cases := []struct {
		name       string
		profileID  string
		ceiling    memlimit.Limit
		wantStart  bool
		wantWhy    bool
		wantInWhy  string
		wantAdvice string
	}{
		{
			name:      "fits under an enforced limit",
			profileID: "tam",
			ceiling:   memlimit.Limit{Bytes: 512 * mb, From: memlimit.SourceCgroupV2},
			wantStart: true,
		},
		{
			name:      "does not fit under an enforced limit, so it will not start",
			profileID: "tam",
			ceiling:   memlimit.Limit{Bytes: 256 * mb, From: memlimit.SourceCgroupV2},
			wantStart: false,
			wantWhy:   true,
			wantInWhy: "Tam Crucible",
			// 256 MB holds Dengeli's 160 floor plus the 48 allowance and
			// not Tam's 320.
			wantAdvice: `"dengeli"`,
		},
		{
			name:      "the same shortfall under cgroup v1 refuses too",
			profileID: "tam",
			ceiling:   memlimit.Limit{Bytes: 256 * mb, From: memlimit.SourceCgroupV1},
			wantStart: false,
			wantWhy:   true,
		},
		{
			name:       "does not fit against free memory, so it warns and starts",
			profileID:  "tam",
			ceiling:    memlimit.Limit{Bytes: 256 * mb, From: memlimit.SourceAvailable},
			wantStart:  true,
			wantWhy:    true,
			wantAdvice: `"dengeli"`,
		},
		{
			name:      "an unknown ceiling starts and says nothing",
			profileID: "tam",
			ceiling:   memlimit.Limit{From: memlimit.SourceUnknown},
			wantStart: true,
		},
		{
			name:      "the smallest profile fits in a very small container",
			profileID: "hafif",
			ceiling:   memlimit.Limit{Bytes: 96 * mb, From: memlimit.SourceCgroupV2},
			wantStart: true,
		},
		{
			name:       "nothing fits at all, and the advice says so",
			profileID:  "tam",
			ceiling:    memlimit.Limit{Bytes: 32 * mb, From: memlimit.SourceCgroupV2},
			wantStart:  false,
			wantWhy:    true,
			wantAdvice: "hiçbir profil",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := budgetVerdict(profileByID(t, tc.profileID), noRateStore, true, tc.ceiling)

			if got.start != tc.wantStart {
				t.Errorf("start = %v, want %v (why: %q)", got.start, tc.wantStart, got.why)
			}
			if (got.why != "") != tc.wantWhy {
				t.Errorf("why = %q, want a reason: %v", got.why, tc.wantWhy)
			}
			if tc.wantInWhy != "" && !strings.Contains(got.why, tc.wantInWhy) {
				t.Errorf("the reason does not name %q:\n%s", tc.wantInWhy, got.why)
			}
			if tc.wantAdvice != "" && !strings.Contains(got.suggestion, tc.wantAdvice) {
				t.Errorf("the suggestion does not mention %q:\n%s",
					tc.wantAdvice, got.suggestion)
			}
			if got.why != "" && got.suggestion == "" {
				t.Error("a refusal or warning with no suggestion: the operator is told " +
					"something is wrong and not what to do about it")
			}
		})
	}
}

// TestARefusalNeverHappensWithoutAnEnforcedLimit is the property stated
// as a property rather than left as a pattern across the table above.
//
// Every profile, against every non-cgroup ceiling, at sizes far below
// what any of them needs: none of it may stop the collector. This is the
// customer's rule - block what would actually crash and nothing else -
// and it is the direction that would be an outage of our own making.
func TestARefusalNeverHappensWithoutAnEnforcedLimit(t *testing.T) {
	estimates := []memlimit.Limit{
		{Bytes: 1 * mb, From: memlimit.SourceAvailable},
		{Bytes: 16 * mb, From: memlimit.SourceAvailable},
		{From: memlimit.SourceUnknown},
		{Bytes: 8 * mb, From: memlimit.SourceUnknown},
	}
	for _, p := range profile.All() {
		for _, ceiling := range estimates {
			v := budgetVerdict(p, 0, true, ceiling)
			if !v.start {
				t.Errorf("profile %q refused to start against a %s ceiling of %d bytes.\n"+
					"Only an enforced cgroup limit may stop the collector; refusing on "+
					"an estimate turns a busy minute into a down site",
					p.ID, ceiling.From, ceiling.Bytes)
			}
		}
	}
}

// TestTheRateStoreCountsAgainstTheBudget.
//
// It is part of Needs, and a limiter set high enough matters: at the
// default 500 requests per second and a 300 second TTL the store is
// bounded at about 23 MB, and a deployment that raised both would be
// carrying hundreds. A budget that ignored it would pass a profile that
// then died with the datasets loaded and the store full - the exact
// failure this file exists for, arriving later and looking unrelated.
func TestTheRateStoreCountsAgainstTheBudget(t *testing.T) {
	tam := profileByID(t, "tam")
	// Just enough for the profile itself and nothing else.
	ceiling := memlimit.Limit{Bytes: tam.Needs(0), From: memlimit.SourceCgroupV2}

	if v := budgetVerdict(tam, 0, true, ceiling); !v.start {
		t.Fatalf("the profile does not fit in exactly what it needs: %s", v.why)
	}
	if v := budgetVerdict(tam, 64*mb, true, ceiling); v.start {
		t.Error("a 64 MB rate store on top of an exactly-sized ceiling was accepted; " +
			"the store is memory this process holds and the budget has to count it")
	}
}
