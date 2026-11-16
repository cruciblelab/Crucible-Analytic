package heartbeat

import (
	"strings"
	"testing"
	"time"
)

func TestARowKeyIsRoleAndInstance(t *testing.T) {
	for _, c := range []struct{ role, instance, key string }{
		{"collector", "", "collector"},
		{"collector", "site-a", "collector@site-a"},
	} {
		if got := RowKey(c.role, c.instance); got != c.key {
			t.Errorf("RowKey(%q, %q) = %q, want %q", c.role, c.instance, got, c.key)
		}
		role, instance := SplitKey(c.key)
		if role != c.role || instance != c.instance {
			t.Errorf("SplitKey(%q) = %q, %q", c.key, role, instance)
		}
	}
	// The role is everything before the first separator - what the policy's
	// split_part reads - so a key cannot name a role the policy did not check.
	if role, _ := SplitKey("collector@a@b"); role != "collector" {
		t.Errorf("SplitKey split at the wrong separator: role %q", role)
	}
}

func keys(beats []Beat) string {
	var k []string
	for _, b := range beats {
		k = append(k, b.Key())
	}
	return strings.Join(k, ",")
}

func TestReadersForgetWhatWasReplacedOrLongSilent(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	row := func(role, instance string, started, beat time.Duration) Beat {
		return Beat{Service: role, Instance: instance, StartedAt: t0.Add(started), BeatAt: t0.Add(beat)}
	}
	for _, c := range []struct {
		name string
		in   []Beat
		want string
	}{
		{"an instance started after the role's row last beat: replaced",
			[]Beat{row("collector", "", -time.Hour, 0), row("collector", "a", time.Second, time.Minute)},
			"collector@a"},
		{"the role's row beat after the instance started: an older build still running",
			[]Beat{row("collector", "", -time.Hour, 2*time.Minute), row("collector", "a", time.Second, time.Minute)},
			"collector,collector@a"},
		{"started at the very instant of the last beat is not after it",
			[]Beat{row("collector", "", -time.Hour, 0), row("collector", "a", 0, time.Minute)},
			"collector,collector@a"},
		{"another role's instance replaces nothing",
			[]Beat{row("collector", "", -time.Hour, 0), row("beacon_writer", "a", time.Second, time.Minute)},
			"collector,beacon_writer@a"},
		{"an instance never replaces an instance",
			[]Beat{row("collector", "b", -time.Hour, 0), row("collector", "a", time.Second, time.Minute)},
			"collector@b,collector@a"},
		{"silent a week and a moment behind its role: forgotten",
			[]Beat{row("collector", "b", -ForgetAfter, -ForgetAfter-time.Second), row("collector", "a", time.Second, 0)},
			"collector@a"},
		{"silent exactly a week: kept",
			[]Beat{row("collector", "b", -ForgetAfter, -ForgetAfter), row("collector", "a", time.Second, 0)},
			"collector@b,collector@a"},
		{"a role every process of which is silent is never forgotten",
			[]Beat{row("collector", "b", -30*24*time.Hour, -30*24*time.Hour), row("beacon_writer", "", 0, 0)},
			"collector@b,beacon_writer"},
	} {
		if got := keys(forgetSilent(forgetReplaced(append([]Beat(nil), c.in...)))); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
