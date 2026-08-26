package web

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// analyticsCallers is the closed list of files in this package allowed
// to talk to the read-only analytics API.
//
// Everything else the panel draws comes from panel_* tables, which is
// what makes "the API is down but the panel still works" true. That is
// currently true by construction rather than by decision, and a claim
// held up by construction stops being true the first time somebody puts
// a visitor count on the site list.
var analyticsCallers = []string{
	"breakdown.go",
	"dashboard.go",
}

// TestOnlyTheAnalyticsPagesTalkToTheAnalyticsAPI.
//
// A structural check rather than a behavioural one, and deliberately so:
// a page that called the API and swallowed the error would pass every
// "does it still answer" test while quietly showing a customer zeroes
// during an outage. This catches the call, not the symptom.
//
// # If this fails because you added a page
//
// That is the point of it firing. Add the file here, and while you are
// here decide what the new page says when the fetch fails - the
// vocabulary is in dashboard.go (unreachable, refused, neverInstalled,
// nothingInRange) and the rule is that none of them may render as "0".
func TestOnlyTheAnalyticsPagesTalkToTheAnalyticsAPI(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	var callers []string
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if strings.Contains(string(body), "s.Analytics") {
			callers = append(callers, name)
		}
	}
	// A rename or a move that emptied this directory would make every
	// assertion below vacuous.
	if scanned < 5 {
		t.Fatalf("only %d source files scanned; this test is not looking at the package", scanned)
	}

	slices.Sort(callers)
	want := slices.Clone(analyticsCallers)
	slices.Sort(want)
	if !slices.Equal(callers, want) {
		t.Errorf("files calling the analytics API are %v, want %v.\n\n"+
			"If you added a page that reads analytics, add it to analyticsCallers - and decide "+
			"what it shows when the fetch fails. The panel's promise is that an outage in the "+
			"read API never takes the panel down and never renders as a zero.", callers, want)
	}
}
