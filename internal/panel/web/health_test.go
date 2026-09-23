package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// heartbeatCounters is every Counter* constant internal/heartbeat
// declares, read from its source.
//
// Read rather than listed. The two tests below used to name the five
// constants by hand under a comment saying "the real constants, not a
// copy" - and a sixth constant added to the heartbeat would have been in
// neither list, so a counter with no label and no place on the page
// would have passed both. Z6 added two.
func heartbeatCounters(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(),
		filepath.Join("..", "..", "heartbeat", "heartbeat.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Counter") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("heartbeat.%s is not a string literal; this test reads the values from source", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, v)
			}
		}
	}
	// Asserted so a scan that stops finding them - a renamed file, a
	// changed prefix - fails rather than passing by examining nothing.
	if len(out) < 7 {
		t.Fatalf("found %d Counter constants in internal/heartbeat, want at least 7: %v", len(out), out)
	}
	return out
}

// TestEveryCounterHasWords is the mirror internal/panel/ui cannot write.
//
// A counter reaches the page as a key joined to a prefix at runtime, so
// neither the template walk nor the source scan sees it. The failure
// this catches is quiet and lands on the worst page for it: a service
// starts reporting a new counter, nobody adds a label, and an operator
// looking at a health page during an incident reads a raw identifier.
func TestEveryCounterHasWords(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	counters := heartbeatCounters(t)
	for _, lang := range catalogs.Languages() {
		for _, c := range counters {
			key := "saglik.sayac." + c
			if !lang.Has(key) {
				t.Errorf("%s: %s has no label", lang.Code, key)
			}
		}
	}
}

// Every counter the page will draw must also be in the order list, or it
// is a number a service reports and the page silently omits.
//
// This is the direction that would otherwise never fail: a counter with
// a perfectly good label, left out of healthCounterOrder, simply does
// not appear - and nothing anywhere says so.
func TestEveryCounterIsDrawn(t *testing.T) {
	inOrder := make(map[string]bool, len(healthCounterOrder))
	for _, c := range healthCounterOrder {
		inOrder[c] = true
	}
	counters := heartbeatCounters(t)
	for _, c := range counters {
		if !inOrder[c] {
			t.Errorf("%q is a counter a service can report and healthCounterOrder does not draw it", c)
		}
	}
	if len(healthCounterOrder) != len(counters) {
		t.Errorf("healthCounterOrder has %d entries and internal/heartbeat declares %d counters; "+
			"it should draw every counter, once, and nothing else", len(healthCounterOrder), len(counters))
	}
	// Dropped first, and this is not cosmetic: it is the only counter
	// that means a customer's numbers are wrong, and a page that buries
	// it under three larger figures has buried the one line that needed
	// reading.
	if healthCounterOrder[0] != heartbeat.CounterDropped {
		t.Errorf("the first counter drawn is %q; dropped is the one that means data was lost",
			healthCounterOrder[0])
	}
}

// Every table the page reports on must have a label, in both languages.
func TestEveryHealthTableHasWords(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	if len(panel.HealthTables) == 0 {
		t.Fatal("panel.HealthTables is empty")
	}
	for _, lang := range catalogs.Languages() {
		for _, table := range panel.HealthTables {
			key := "saglik.tablo." + table
			if !lang.Has(key) {
				t.Errorf("%s: %s has no label", lang.Code, key)
			}
		}
	}
}

// And every service role, which is what the heartbeat table is keyed by.
func TestEveryServiceRoleHasWords(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range catalogs.Languages() {
		for _, role := range []string{"collector", "beacon_writer", "analytics_reader", "panel_user"} {
			key := "saglik.servis." + role
			if !lang.Has(key) {
				t.Errorf("%s: %s has no label", lang.Code, key)
			}
		}
	}
}

// TestTheHealthPageCarriesNoVisitorNumbers.
//
// The page's promise, checked structurally. Its own types are the whole
// surface a template can address, so a field that named a traffic
// quantity would be a route from the panel's role to the analytics it is
// specifically not allowed to read - through a page nobody would think
// to check, because the panel is supposed to read this table.
//
// By field name rather than by value: a value test would need the field
// to exist first, which is one commit too late.
func TestTheHealthPageCarriesNoVisitorNumbers(t *testing.T) {
	// Words that would mean a number about traffic rather than about a
	// process. "written" and "dropped" are about rows this process
	// handled and are fine; "visitors" and "pageviews" are not.
	forbidden := []string{
		"visitor", "ziyaretci", "pageview", "goruntuleme",
		"session", "oturum", "bounce", "referrer", "kaynak",
		"country", "ulke", "path", "sayfa", "useragent", "ip",
	}
	for _, typeName := range []string{"healthPage", "healthService", "healthStorage", "healthCounter", "healthAPI"} {
		for _, field := range structFieldNames(t, "health.go", typeName) {
			lower := strings.ToLower(field)
			for _, word := range forbidden {
				if strings.Contains(lower, word) {
					t.Errorf("%s has a field named %q; nothing on the health page may be a number "+
						"about a visitor - that is the isolation this panel's whole design rests on",
						typeName, field)
				}
			}
		}
	}
}
