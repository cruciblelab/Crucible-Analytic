package main

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/configreport"
)

// What the beacon says in its heartbeat (PLAN §W4, items 5 and 6).
func TestTheBeaconsReport(t *testing.T) {
	cfg := beacon.Config{ListenAddr: "127.0.0.1:8081", Sites: []string{"dosya-a", "dosya-b"},
		TimescaleDSN: "postgres://beacon_writer:hunter2@db/analytics"}
	srv := &beacon.Server{}

	// Before the settings have been applied: no word about the sites. The
	// file's list may be about to be replaced by the panel's.
	r := beaconConfigReport(cfg, srv.PathPrefixInForce(), nil)
	if r.Program != configreport.Beacon {
		t.Errorf("program = %q", r.Program)
	}
	if _, said := r.Values[configreport.KeySitesSource]; said {
		t.Error("the beacon named a site source before it had applied its settings")
	}
	// The prefix in force, not the file's empty value: the wizard showed
	// "" for a beacon mounted at /_ca.
	if got := r.Values["path_prefix"]; got != beacon.DefaultPathPrefix {
		t.Errorf("path_prefix = %q, want the prefix in force %q", got, beacon.DefaultPathPrefix)
	}
	for k, v := range r.Values {
		if v == cfg.TimescaleDSN {
			t.Errorf("%s carries the DSN", k)
		}
	}

	for _, c := range []struct {
		fromPanel bool
		want      string
	}{{false, configreport.SourceFile}, {true, configreport.SourcePanel}} {
		r = beaconConfigReport(cfg, "/_ca", &acceptedSites{sites: []string{"a", "b"}, fromPanel: c.fromPanel})
		if got := r.Values[configreport.KeySitesSource]; got != c.want {
			t.Errorf("fromPanel %v: source = %q, want %q", c.fromPanel, got, c.want)
		}
		if got := r.Values[configreport.KeyAcceptedSites]; got != "a, b" {
			t.Errorf("accepted = %q, want %q", got, "a, b")
		}
	}

	// What SetSites would not take is not reported as accepted.
	if got := withoutRefused([]string{"a", "bad", "b"}, []string{"bad"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("withoutRefused = %v, want [a b]", got)
	}
}
