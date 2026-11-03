package web

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// TestTheExampleConfigNamesTheDefaultServiceRoles: panel.example.toml's
// [roles] and the store's defaults are the same two names, so a
// deployment that copies the example and one that leaves [roles] out
// read the same heartbeat rows.
func TestTheExampleConfigNamesTheDefaultServiceRoles(t *testing.T) {
	cfg, err := LoadConfig("../../../panel.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Collector != panel.DefaultServiceRoles.Collector || cfg.Roles.Beacon != panel.DefaultServiceRoles.Beacon {
		t.Errorf("panel.example.toml names %q/%q, the store defaults to %q/%q",
			cfg.Roles.Collector, cfg.Roles.Beacon, panel.DefaultServiceRoles.Collector, panel.DefaultServiceRoles.Beacon)
	}
}
