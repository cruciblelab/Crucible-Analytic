package preflight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A question that was never put must not look like a question with no
// answer.
//
// checkService is driven entirely by Config.ServiceURLs, so before this
// an empty map produced no service check at all - and cmd/panel passed
// an empty map, because the field existed and nothing filled it. The
// wizard listed fourteen checks about the schema, the roles, the disk
// and the log tree, and not one request to a service, and no line in it
// said so. A checklist cannot show the absence of an entry nobody
// wrote.
func TestAWizardWithNoServiceAddressesSaysSo(t *testing.T) {
	results := New(nil, false).Run(context.Background(), Config{})

	var found *CheckResult
	for i := range results {
		if results[i].ID == "service.configured" {
			found = &results[i]
		}
		if strings.HasPrefix(results[i].ID, "service.") && results[i].ID != "service.configured" {
			t.Errorf("%s was produced without any address being configured", results[i].ID)
		}
	}
	if found == nil {
		t.Fatal("a run with no service addresses produced no service line at all.\n" +
			"That is the defect this test exists for: the wizard then reports on " +
			"everything except whether the services are running, and says nothing " +
			"about the omission.")
	}
	if found.Status != CheckWarn {
		t.Errorf("service.configured = %s, want warn: nothing is broken, it was just "+
			"not asked", found.Status)
	}
	if found.Label == "" || found.Detail == "" || found.Fix == "" {
		t.Error("service.configured came back without a label, detail or fix; a row " +
			"that cannot say what to do is a row the installer scrolls past")
	}
	if !strings.Contains(found.Fix, "service_urls") {
		t.Errorf("the fix does not name the setting that would answer it: %q", found.Fix)
	}
}

// And it does not block handover.
//
// Deliberate, and it is the owner's rule rather than a convenience: do
// not block on something that is not doing real harm - warn, and leave
// it. A deployment whose three services are all running is not broken
// because panel.toml does not list their addresses.
//
// Asserted on its own rather than inside the test above, because the
// two claims fail for different reasons: one is "the line is missing"
// and the other is "the line is too severe".
func TestTheMissingAddressWarningDoesNotBlockHandover(t *testing.T) {
	warning := noServiceURLs()
	if ok, blocking := Complete([]CheckResult{warning}); !ok {
		t.Errorf("handover blocked by %d check(s), the first being %q.\n"+
			"Severity is %s and Status is %s; a recommended warning must never "+
			"block, or an installer with three healthy services cannot finish "+
			"the wizard.", len(blocking), blocking[0].ID, warning.Severity, warning.Status)
	}
}

// A configured address is fetched, and the line it produces is required.
//
// The warning above is the "nobody asked" case. This is the other half:
// once somebody has asked, an unreachable service blocks handover -
// which is what makes configuring it worth doing.
func TestAConfiguredServiceIsActuallyFetched(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	results := New(nil, false).Run(context.Background(), Config{
		ServiceURLs: map[string]string{
			"beacon": up.URL + "/healthz",
			// A port nothing listens on, rather than a name that does
			// not resolve: DNS failures take as long as the resolver
			// decides to, and a check with a five second budget would
			// then be measuring the resolver.
			"api": "http://127.0.0.1:1/healthz",
		},
	})

	if hits == 0 {
		t.Error("the configured address was never requested; the check reported on a " +
			"service it did not contact")
	}

	byID := map[string]CheckResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if _, configured := byID["service.configured"]; configured {
		t.Error("the not-configured warning appeared alongside configured services")
	}
	if got := byID["service.beacon"]; got.Status != CheckPass {
		t.Errorf("service.beacon = %s, want pass: %s", got.Status, got.Detail)
	}
	if got := byID["service.api"]; got.Status != CheckFail {
		t.Errorf("service.api = %s, want fail: %s", got.Status, got.Detail)
	}
	if ok, _ := Complete([]CheckResult{byID["service.api"]}); ok {
		t.Error("a service that does not answer would not block handover, which leaves " +
			"the check with nothing to enforce")
	}
}
