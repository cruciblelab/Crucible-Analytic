//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const (
	w3Own   = "w3-kendi"
	w3Other = "w3-baska"
)

// writeInstanceBeat writes one heartbeat as the collector's instance for
// site, through the reporter, and waits for the row.
func writeInstanceBeat(t *testing.T, store *panel.Store, site string) {
	t.Helper()
	r := heartbeat.New(heartbeat.Options{
		Pool:     testdb.Pool(t, testdb.Collector),
		Version:  "w3-" + site,
		Instance: site,
		Started:  time.Now(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := store.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM service_heartbeat WHERE service = $1`,
			heartbeat.RowKey(testdb.Collector, site)).Scan(&n); err == nil && n == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the collector's row for %s never appeared", site)
}

// TestAnInstanceRowIsShownToWhoeverOwnsItsSite (W3).
//
// A collector's instance is named after the site it fronts, so its row on
// the health page and in the diagnostic file says which sites a
// deployment serves - and, through its counters, how busy each one is.
// On a deployment shared by two customers, the owner of one site is shown
// her own collector and not the other customer's; the operator is shown
// both.
func TestAnInstanceRowIsShownToWhoeverOwnsItsSite(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyBeaconSites)
	admin := testdb.Admin(t)
	// After setupSoleTenantServer's AccountsLock, the order every suite
	// holding both takes them in; before the cleanup, so the rows go while
	// it is held.
	testdb.Lock(t, admin, testdb.HeartbeatLock)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM service_heartbeat WHERE version LIKE 'w3-%'`)
	})

	owner := makeUser(t, store, "w3-sahip", false)
	other := makeUser(t, store, "w3-oteki", false)
	operator := makeUser(t, store, "w3-isletmeci", true)
	for site, who := range map[string]panel.User{w3Own: owner, w3Other: other} {
		if err := store.AddMember(ctx, site, who.ID, panel.RoleOwner, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{w3Own, w3Other}, nil); err != nil {
		t.Fatal(err)
	}
	writeInstanceBeat(t, store, w3Own)
	writeInstanceBeat(t, store, w3Other)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	instances := func(c *http.Client) (page string, file []string) {
		t.Helper()
		_, page = get(t, c, server.URL+HealthPath)
		for _, row := range diagnosticOf(t, c, server.URL).Services.Rows {
			if row.Instance != "" {
				file = append(file, row.Instance)
			}
		}
		return page, file
	}

	page, file := instances(signedIn(t, server.URL, owner.Email))
	if !strings.Contains(page, "<code>"+w3Own+"</code>") {
		t.Error("the owner of one site is not shown her own collector")
	}
	if strings.Contains(page, w3Other) {
		i := strings.Index(page, w3Other)
		t.Errorf("the owner of one site is shown the other customer's collector on the health page: ...%s...", page[max(0, i-300):min(len(page), i+100)])
	}
	// The whole file, not its services section alone: the first version
	// leaked the other site through the setup checks' token-key sentence.
	if _, raw := get(t, signedIn(t, server.URL, owner.Email), server.URL+DiagnosticPath); strings.Contains(raw, w3Other) {
		i := strings.Index(raw, w3Other)
		t.Errorf("the owner of one site's diagnostic file names the other site: ...%s...", raw[max(0, i-200):min(len(raw), i+50)])
	}
	if strings.Join(file, ",") != w3Own {
		t.Errorf("the owner of one site's diagnostic file carries instances %v, want only %s", file, w3Own)
	}

	page, file = instances(signedIn(t, server.URL, operator.Email))
	for _, site := range []string{w3Own, w3Other} {
		if !strings.Contains(page, "<code>"+site+"</code>") {
			t.Errorf("the operator is not shown the collector for %s", site)
		}
	}
	if len(file) != 2 {
		t.Errorf("the operator's diagnostic file carries instances %v, want both", file)
	}

	// The restart section (W3b): the operator may restart either process
	// alone, or the role whole - and that option says it is more than one.
	for _, site := range []string{w3Own, w3Other} {
		if !strings.Contains(page, `<option value="`+testdb.Collector+`@`+site+`"`) {
			t.Errorf("the operator is not offered a restart of the collector for %s alone", site)
		}
	}
	if !strings.Contains(page, `<option value="`+testdb.Collector+`">`) ||
		!strings.Contains(page, "hepsi (") {
		t.Error("the operator's option to restart every collector does not say it is all of them")
	}
}
