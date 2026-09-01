//go:build integration

package preflight

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// A hand-run demonstration against whatever is actually deployed, so the
// output can be read rather than inferred from assertions.
//
//	CRUCIBLE_PREFLIGHT_DEMO=/var/log/crucible-analytic go test -tags integration \
//	    ./internal/panel/preflight/ -run TestPreflightDemo -v
func TestPreflightDemo(t *testing.T) {
	logDir := os.Getenv("CRUCIBLE_PREFLIGHT_DEMO")
	if logDir == "" {
		t.Skip("set CRUCIBLE_PREFLIGHT_DEMO to the log directory to run this")
	}
	c := newTestChecker(t)

	// Built from whatever hash the deployment actually has, so the run
	// shows the real state rather than skipping the check. An empty
	// CRUCIBLE_DEV_PASSWORD_HASH is itself a real state - it is what a
	// deployment that never ran cmd/devpass looks like - and the warning
	// it produces is the informative one.
	gate, err := devgate.New(
		devgate.Config{PasswordHash: os.Getenv("CRUCIBLE_DEV_PASSWORD_HASH")},
		devgate.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("devgate.New: %v", err)
	}

	results := c.Run(context.Background(), Config{
		LogDir:  logDir,
		DataDir: "/",
		ServiceURLs: map[string]string{
			"beacon": "http://127.0.0.1:8081/healthz",
			"api":    "http://127.0.0.1:8080/healthz",
		},
		Roles:         Roles{Beacon: "beacon_writer", API: "analytics_reader", Panel: "panel_user"},
		DeveloperGate: gate,
	})

	icon := map[CheckStatus]string{CheckPass: "OK  ", CheckFail: "HATA", CheckWarn: "UYAR", CheckSkip: "----"}
	for _, r := range results {
		t.Logf("%s  %-40s %s", icon[r.Status], r.Label, r.Detail)
		if r.Fix != "" && r.Status != CheckPass {
			t.Logf("          → %s", r.Fix)
		}
	}
	ok, blocking := Complete(results)
	if ok {
		t.Log("KURULUM TAMAMLANABİLİR — zorunlu kontrollerin hepsi geçti")
	} else {
		t.Logf("KURULUM TAMAMLANAMAZ — %d zorunlu kontrol başarısız", len(blocking))
	}
}
