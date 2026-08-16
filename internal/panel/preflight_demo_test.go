//go:build integration

package panel

import (
	"context"
	"os"
	"testing"
)

// A hand-run demonstration against whatever is actually deployed, so the
// output can be read rather than inferred from assertions.
//
//	CRUCIBLE_PREFLIGHT_DEMO=/var/log/crucible go test -tags integration \
//	    ./internal/panel/ -run TestPreflightDemo -v
func TestPreflightDemo(t *testing.T) {
	logDir := os.Getenv("CRUCIBLE_PREFLIGHT_DEMO")
	if logDir == "" {
		t.Skip("set CRUCIBLE_PREFLIGHT_DEMO to the log directory to run this")
	}
	store := newTestStore(t, "demo")

	results := store.RunPreflight(context.Background(), PreflightConfig{
		LogDir:  logDir,
		DataDir: "/",
		ServiceURLs: map[string]string{
			"beacon": "http://127.0.0.1:8081/healthz",
			"api":    "http://127.0.0.1:8080/healthz",
		},
		Roles: PreflightRoles{Beacon: "beacon_writer", API: "analytics_reader", Panel: "panel_user"},
	})

	icon := map[CheckStatus]string{CheckPass: "OK  ", CheckFail: "HATA", CheckWarn: "UYAR", CheckSkip: "----"}
	for _, r := range results {
		t.Logf("%s  %-40s %s", icon[r.Status], r.Label, r.Detail)
		if r.Fix != "" && r.Status != CheckPass {
			t.Logf("          → %s", r.Fix)
		}
	}
	ok, blocking := PreflightComplete(results)
	if ok {
		t.Log("KURULUM TAMAMLANABİLİR — zorunlu kontrollerin hepsi geçti")
	} else {
		t.Logf("KURULUM TAMAMLANAMAZ — %d zorunlu kontrol başarısız", len(blocking))
	}
}
