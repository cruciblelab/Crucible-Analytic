package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The crossover queries must join on the shared key expression, never on
// a bare address column.
//
// This is a structural check on the source because the failure it guards
// is invisible at runtime, and the reason it is invisible changed once
// without this comment noticing.
//
// It used to say a deployment storing pseudonyms has NULL in `ip`, so a
// bare join returns nothing. That has not been true since full mode
// replaced hashed mode: `ip` always holds the masked network. The
// failure is quieter now, which is worse. A query joining `b.ip = c.ip`
// in full mode returns plausible numbers computed at /24 precision -
// every visitor sharing a /24 collapsed into one - while the row beside
// it, computed through joinKey, counts them apart. Two numbers on one
// page, disagreeing, neither of them flagged.
//
// The next person to add a query here will reach for the obvious
// spelling, and this is what tells them not to.
func TestCrossoverQueries_JoinOnTheSharedKey(t *testing.T) {
	source, err := os.ReadFile("store_crossover.go")
	if err != nil {
		t.Fatalf("reading the crossover source: %v", err)
	}
	text := stripCrossoverComments(string(source))

	// Any comparison of two ip columns, in either direction.
	bare := regexp.MustCompile(`\b[a-z]\.ip\s*=\s*[a-z]\.ip\b`)
	if found := bare.FindAllString(text, -1); len(found) > 0 {
		t.Errorf("crossover queries join on a bare address column: %v\n"+
			"use the joinKey expression instead - in full mode that column holds only "+
			"the /24, so such a join quietly answers a coarser question than the "+
			"queries beside it", found)
	}

	// And the shared expression is actually in use, so this test cannot
	// pass merely because somebody deleted every join.
	if !strings.Contains(text, "joinKey") {
		t.Error("no query references joinKey; either the expression was removed or the joins were")
	}
	if strings.Count(text, "+ joinKey +") < 4 {
		t.Errorf("joinKey appears in %d places; the four CTEs that group or "+
			"select by address should each use it", strings.Count(text, "+ joinKey +"))
	}
}

// stripCrossoverComments removes // comments so that documenting the old
// spelling - as the joinKey comment does - is not mistaken for using it.
func stripCrossoverComments(source string) string {
	var b strings.Builder
	for _, line := range strings.Split(source, "\n") {
		if i := strings.Index(strings.TrimSpace(line), "//"); i == 0 {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
