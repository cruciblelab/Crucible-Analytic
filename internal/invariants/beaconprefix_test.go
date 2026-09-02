package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The beacon is the one server in this repository whose URL space is not
// ours. It sits behind the customer's own reverse proxy, and README's
// deployment section forwards exactly one path to it:
//
//	location /_ca/ {
//	    proxy_pass http://127.0.0.1:8081;
//	}
//
// docker/compose.yml says the same thing a different way - it publishes
// 8081 and tells the operator to "put it behind the same reverse proxy
// that terminates TLS for the site".
//
// So a route registered outside that prefix is not merely unconventional.
// It is unreachable on every deployment that already exists, and it stays
// unreachable until somebody edits an nginx file on each of them. A
// feature shipped that way looks broken to the customer and looks fine
// here, because nothing in this repository forwards anything.
//
// Written before the routes it is meant to protect. The visitor-facing
// privacy surface (PLAN.md §P) adds endpoints, and the decision that they
// live under the prefix is worth more as a check that already exists than
// as a paragraph somebody rereads.
//
// # The exceptions, and why a list rather than a rule
//
// One route is legitimately outside the prefix, and its reason does not
// generalise: /healthz answers the container's own healthcheck over
// loopback, and a visitor never asks for it. That is a judgement about
// one route's audience, which no pattern can make - so it is a list, each
// entry carrying the reason it is allowed to be where it is.
var beaconRoutesOutsidePrefix = map[string]string{
	`"GET /healthz"`: "the container healthcheck in docker/compose.yml asks for this " +
		"over loopback; it is not a visitor-facing route and must not move under a " +
		"prefix the customer's proxy owns",
}

// handleFunc pulls the pattern out of each mux registration. The pattern
// is everything up to the first comma, which holds because every
// registration in this file passes a string expression and then a
// handler.
var handleFunc = regexp.MustCompile(`mux\.HandleFunc\(([^,]+),`)

// TestEveryVisitorFacingBeaconRouteIsUnderThePathPrefix.
func TestEveryVisitorFacingBeaconRouteIsUnderThePathPrefix(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal", "beacon", "server.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := handleFunc.FindAllStringSubmatch(string(body), -1)
	if len(found) < 3 {
		t.Fatalf("only %d routes found in internal/beacon/server.go; the pattern has "+
			"stopped matching how they are registered, so this test is checking nothing",
			len(found))
	}

	seen := map[string]bool{}
	for _, m := range found {
		pattern := strings.TrimSpace(m[1])
		seen[pattern] = true

		// Under the prefix means built from the same variable the other
		// routes use, not from a second literal that happens to say
		// "/_ca" today.
		if strings.Contains(pattern, "prefix") {
			continue
		}
		if why, ok := beaconRoutesOutsidePrefix[pattern]; ok {
			t.Logf("outside the prefix on purpose: %s - %s", pattern, why)
			continue
		}
		t.Errorf("the beacon registers %s outside its path prefix and the list in this "+
			"file does not say why.\n"+
			"The customer's reverse proxy forwards one prefix to this process, so a "+
			"route outside it is unreachable on every deployment that already exists. "+
			"Build the pattern from `prefix`, or add it to beaconRoutesOutsidePrefix "+
			"with the reason a visitor never needs it", pattern)
	}

	// The other direction: an exception for a route that no longer
	// exists is a reason nobody can check, and the next person to read
	// the list would believe it.
	for pattern := range beaconRoutesOutsidePrefix {
		if !seen[pattern] {
			t.Errorf("beaconRoutesOutsidePrefix excuses %s and no such route is "+
				"registered any more", pattern)
		}
	}
}

// TestTheDocumentedProxyRuleMatchesTheDefaultPrefix.
//
// The nginx block in README is the deployment instruction every installed
// system followed once and will not revisit. If DefaultPathPrefix ever
// changed without it, existing deployments would forward the old path to
// a process listening on the new one - and the symptom is a snippet that
// 404s on sites that were working yesterday.
func TestTheDocumentedProxyRuleMatchesTheDefaultPrefix(t *testing.T) {
	root := repoRoot(t)

	server := readTextFile(t, filepath.Join(root, "internal", "beacon", "server.go"))
	prefix := regexp.MustCompile(`DefaultPathPrefix\s*=\s*"([^"]+)"`).FindStringSubmatch(server)
	if prefix == nil {
		t.Fatal("DefaultPathPrefix is not declared in internal/beacon/server.go the way " +
			"this test reads it")
	}

	readme := readTextFile(t, filepath.Join(root, "README.md"))
	want := "location " + prefix[1] + "/ {"
	if !strings.Contains(readme, want) {
		t.Errorf("README does not carry the nginx rule for the default prefix %q.\n"+
			"Looked for: %s\n"+
			"Whoever follows the README is configuring the path this build does not "+
			"serve", prefix[1], want)
	}
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
