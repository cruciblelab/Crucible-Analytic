package beacon

import (
	"strings"
	"testing"
)

// [event_rate] from the file: unset is the default, a written number is
// kept, a negative one turns that limit off. Through LoadConfig, so the
// section's TOML names are what is tested, not the struct's.
func TestLoadConfig_EventRate(t *testing.T) {
	for _, c := range []struct {
		name, toml string
		want       EventRateLimits
	}{
		{"unset", "", DefaultEventRate},
		{"written", "[event_rate]\nmark_burst = 400\nmark_per_minute = 9000\ndrop_burst = 4000\ndrop_per_minute = 90000\n",
			EventRateLimits{MarkBurst: 400, MarkPerMinute: 9000, DropBurst: 4000, DropPerMinute: 90000}},
		{"one off", "[event_rate]\ndrop_per_minute = -1\n",
			EventRateLimits{MarkBurst: 50, MarkPerMinute: 15000, DropBurst: 200, DropPerMinute: 0}},
		{"all off", "[event_rate]\nmark_burst = -1\nmark_per_minute = -1\ndrop_burst = -1\ndrop_per_minute = -1\n",
			EventRateLimits{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadConfig(beaconTOML(t, c.toml))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.EventRate.Limits(); got != c.want {
				t.Errorf("limits %+v, want %+v", got, c.want)
			}
		})
	}
}

// A drop at or under its own mark means nothing is ever marked; the file
// is refused, naming the window. A drop under the mark of the *other*
// window is a file that means what it says and is kept, and so is a
// window whose mark is off.
func TestLoadConfig_EventRateDropMustBeAboveItsMark(t *testing.T) {
	for _, c := range []struct {
		toml, refusedFor string
	}{
		{"[event_rate]\nmark_burst = 50\ndrop_burst = 50\n", "drop_burst"},
		{"[event_rate]\nmark_per_minute = 5000\ndrop_per_minute = 4999\n", "drop_per_minute"},
		// Against the default mark (50 in 100 ms).
		{"[event_rate]\ndrop_burst = 50\n", "drop_burst"},
		{"[event_rate]\nmark_burst = 50\ndrop_burst = 51\n", ""},
		{"[event_rate]\nmark_per_minute = 5000\ndrop_burst = 200\n", ""},
		{"[event_rate]\nmark_burst = -1\ndrop_burst = 10\n", ""},
	} {
		_, err := LoadConfig(beaconTOML(t, c.toml))
		switch {
		case c.refusedFor == "" && err != nil:
			t.Errorf("%q refused: %v", c.toml, err)
		case c.refusedFor != "" && err == nil:
			t.Errorf("%q accepted", c.toml)
		case c.refusedFor != "" && !strings.Contains(err.Error(), "event_rate."+c.refusedFor):
			t.Errorf("%q refused for the wrong reason: %v", c.toml, err)
		}
	}
}
