package collector

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/profile"
)

// TestProfileLevelIsDerivedFromTheSettingsThatCostMemory.
//
// # Why this is a derivation and not a stored value
//
// A2's rule: the profile is a name for what a configuration already is,
// never a mode the code branches on and never a value written down
// beside the settings it summarises. A stored name goes stale the first
// time somebody edits one setting by hand, and then two places disagree
// about what the deployment is doing - with the panel showing the one
// that is wrong, because the panel would be reading the label.
//
// So the only thing that can be wrong here is this function, and the
// table below is what says it is not.
//
// # The case that looks redundant and is not
//
// country_only with the lookup disabled. Both spellings of "off" have to
// give the same answer, because a configuration can carry a leftover
// country_only from before somebody switched the lookup off, and
// reporting that as the country profile would claim 59 MB are loaded
// when nothing is.
func TestProfileLevelIsDerivedFromTheSettingsThatCostMemory(t *testing.T) {
	cases := []struct {
		name        string
		enabled     bool
		countryOnly bool
		want        profile.Level
		wantID      string
	}{
		{
			name: "the lookup is off", enabled: false, countryOnly: false,
			want: profile.LevelOff, wantID: "hafif",
		},
		{
			name:    "the lookup is off and country_only was left behind",
			enabled: false, countryOnly: true,
			want: profile.LevelOff, wantID: "hafif",
		},
		{
			name: "country data only", enabled: true, countryOnly: true,
			want: profile.LevelCountry, wantID: "dengeli",
		},
		{
			name: "both datasets", enabled: true, countryOnly: false,
			want: profile.LevelFull, wantID: "tam",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.ASNLookup.Enabled = tc.enabled
			cfg.ASNLookup.CountryOnly = tc.countryOnly

			if got := cfg.ProfileLevel(); got != tc.want {
				t.Errorf("ProfileLevel() = %q, want %q", got, tc.want)
			}
			got, ok := cfg.Profile()
			if !ok {
				t.Fatalf("no profile matches level %q; internal/profile and this "+
					"derivation have stopped agreeing about what levels exist",
					cfg.ProfileLevel())
			}
			if got.ID != tc.wantID {
				t.Errorf("Profile() = %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

// TestEveryProfileIsReachableFromAConfiguration is the other direction,
// and it is the one that catches a profile nobody can actually select.
//
// A fourth profile added to internal/profile with no configuration that
// produces it would show up in the panel's list, cost somebody a
// decision, and then never be what any deployment is. The failure is
// quiet: everything works, one option is a lie.
func TestEveryProfileIsReachableFromAConfiguration(t *testing.T) {
	reached := map[string]bool{}
	for _, enabled := range []bool{false, true} {
		for _, countryOnly := range []bool{false, true} {
			cfg := &Config{}
			cfg.ASNLookup.Enabled = enabled
			cfg.ASNLookup.CountryOnly = countryOnly
			if p, ok := cfg.Profile(); ok {
				reached[p.ID] = true
			}
		}
	}

	for _, p := range profile.All() {
		if !reached[p.ID] {
			t.Errorf("no combination of asn_lookup settings produces the %q profile, "+
				"so it is offered and cannot be chosen", p.ID)
		}
	}
}

// TestRateStoreBoundFollowsTheLimiter.
//
// The bound is the limiter's, not the store's, and that is the whole
// finding it rests on: internal/proxy admits a connection past the geo
// list and the rate limit before RecordRequest is ever called, so the
// map cannot grow past one entry per admitted request per TTL however
// many addresses an attacker brings.
//
// With the limit off there is no such ceiling. Returning a number there
// would be worse than returning nothing, because the number would look
// like a bound and be used as one.
func TestRateStoreBoundFollowsTheLimiter(t *testing.T) {
	cfg := &Config{}
	cfg.Limits.MaxRequestsPerSecond = 500
	cfg.Cache.TTLSeconds = 300

	bytes, bounded := cfg.RateStoreBound()
	if !bounded {
		t.Fatal("a configured requests-per-second limit did not produce a bound")
	}
	// 500 * 300 * 160 bytes, which is where the ~23 MB in the design
	// notes comes from. Asserted as a range rather than an equality so
	// this does not become a test of the multiplication.
	if bytes < 20<<20 || bytes > 26<<20 {
		t.Errorf("bound = %d bytes (%.1f MB), want roughly 23 MB for 500 rps over "+
			"300 seconds", bytes, float64(bytes)/(1<<20))
	}

	cfg.Limits.MaxRequestsPerSecond = 0
	if _, bounded := cfg.RateStoreBound(); bounded {
		t.Error("the limit is off and the bound was reported as known; with nothing " +
			"rejecting requests there is no ceiling on how many addresses reach the store")
	}
}
