package beacon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// beaconTOML writes a config file with the two required fields filled
// in, plus whatever the caller adds.
func beaconTOML(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beacon.toml")
	// Both required fields at the top level, and extra after them: a
	// TOML table header would swallow any bare key that followed it.
	body := `
sites = ["bir-site", "iki-site"]
timescale_dsn = "postgres://localhost/test"
` + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadConfig_RetentionOutsideTheBoundsIsRefused.
//
// Retention moved out of the panel and into this file, and the ceiling
// came down from ten years to two on the way. Both halves of that make
// this test necessary: a file written against an older build can now be
// out of range, and the old behaviour for out of range was to fall back
// to 90 days without saying anything.
//
// A deployment that believes it keeps five years while keeping three
// months finds out when somebody asks for last year's figures. Refusing
// to start is the only report that cannot be missed.
//
// 3650 is in the table on purpose: it was the previous ceiling, so it is
// the exact number an existing deployment is most likely to have.
func TestLoadConfig_RetentionOutsideTheBoundsIsRefused(t *testing.T) {
	for _, days := range []int{-1, 731, 3650, 20000} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			path := beaconTOML(t, "[retention]\ndays = "+strconv.Itoa(days)+"\n")
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig accepted retention.days = %d", days)
			}
			// Which error, not just that there was one. The first
			// version of this file put the required fields under a
			// table header where they were not read, so every case here
			// passed on "timescale_dsn is required" and the bounds were
			// never exercised at all.
			if !strings.Contains(err.Error(), "retention.days") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// TestLoadConfig_RetentionInsideTheBoundsIsKept, at both ends and unset.
func TestLoadConfig_RetentionInsideTheBoundsIsKept(t *testing.T) {
	for _, tc := range []struct{ days, want int }{
		{0, DefaultRetentionDays}, // unset
		{1, 1},                    // the floor
		{90, 90},                  // the default, written out
		{730, 730},                // the ceiling
	} {
		t.Run(strconv.Itoa(tc.days), func(t *testing.T) {
			path := beaconTOML(t, "[retention]\ndays = "+strconv.Itoa(tc.days)+"\n")
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got := cfg.Retention.Resolved(); got != tc.want {
				t.Errorf("Resolved() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRetentionPolicy_PerSiteSurvivedTheMoveOutOfThePanel.
//
// Per-site retention was the panel's, and "this customer asked for
// thirty days" is a real request rather than a feature nobody used - so
// removing the panel setting had to relocate it, not drop it. Without
// this the row-delete path in internal/retention would have no way to be
// reached at all, which is a quieter kind of removal.
func TestRetentionPolicy_PerSiteSurvivedTheMoveOutOfThePanel(t *testing.T) {
	path := beaconTOML(t, `
[retention]
days = 90
per_site = { "bir-site" = 30 }
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	policy := cfg.RetentionPolicy()
	if policy.Days != 90 {
		t.Errorf("deployment-wide retention = %d, want 90", policy.Days)
	}
	if got, ok := policy.PerSite["bir-site"]; !ok || got != 30 {
		t.Errorf("bir-site = %d (present %v), want 30", got, ok)
	}
	if _, ok := policy.PerSite["iki-site"]; ok {
		t.Error("a site with no entry of its own was given one; it should fall through to Days")
	}
}

// TestRetentionPolicy_ASiteAskingForTheDeploymentsOwnFigureIsNotAnOverride.
//
// An entry equal to the deployment-wide number would put that site on
// the row-delete path for no reason - the hypertable already drops those
// chunks - so it is dropped rather than carried. Cheap to get wrong and
// invisible when it is: the numbers would be right and the deployment
// would scan and delete for nothing, hourly, forever.
func TestRetentionPolicy_ASiteAskingForTheDeploymentsOwnFigureIsNotAnOverride(t *testing.T) {
	path := beaconTOML(t, `
[retention]
days = 90
per_site = { "bir-site" = 90 }
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.RetentionPolicy().PerSite) != 0 {
		t.Errorf("PerSite = %v, want empty", cfg.RetentionPolicy().PerSite)
	}
}

// TestLoadConfig_PerSiteRetentionIsValidatedToo. The bounds are the
// point of the ceiling; an override is exactly where somebody would
// expect to escape it.
func TestLoadConfig_PerSiteRetentionIsValidatedToo(t *testing.T) {
	for name, extra := range map[string]string{
		"over the ceiling":   "[retention]\nper_site = { \"bir-site\" = 3650 }\n",
		"under the floor":    "[retention]\nper_site = { \"bir-site\" = 0 }\n",
		"site id with slash": "[retention]\nper_site = { \"bir/site\" = 30 }\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(beaconTOML(t, extra))
			if err == nil {
				t.Fatalf("LoadConfig accepted %s", name)
			}
			if !strings.Contains(err.Error(), "per_site") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}
