package api

import (
	"os"
	"regexp"
	"testing"
)

// The per-address country probe says which row it wants, rather than
// taking whichever one arrives.
//
// # Why this is a source check and not an assertion about results
//
// The probe is a LATERAL with LIMIT 1, and "the most recent snapshot for
// this address" is expressed by its ORDER BY. Delete that clause and
// every test still passes - measured, not assumed. traffic_snapshots is
// indexed on (ip, time DESC), so any plan that uses the index hands back
// the newest row first whether or not the query asked for it, and with
// real data the planner always uses it.
//
// That the clause is load-bearing was measured separately, on a table
// with no index and the older row written first:
//
//	with ORDER BY     TR   (the later country, correct)
//	without           DE   (the earlier one)
//
// So the guarantee is real and this repository's own tests cannot see
// it. What they can see is whether the query still states it - which is
// the thing that must survive somebody reworking the index, and exactly
// the case where a result-based test would go quietly wrong instead of
// red.
func TestTheCountryProbeAsksForTheMostRecentRow(t *testing.T) {
	source, err := os.ReadFile("store_beacon.go")
	if err != nil {
		t.Fatalf("reading the beacon query source: %v", err)
	}
	text := string(source)

	lateral := regexp.MustCompile(`(?is)LEFT JOIN LATERAL \((.*?)\) g ON true`)
	found := lateral.FindAllStringSubmatch(text, -1)
	if len(found) == 0 {
		t.Fatal("no LATERAL probe found in store_beacon.go; either the country fallback was " +
			"rewritten or this check stopped matching how it is spelled, and a check that " +
			"matches nothing reports nothing")
	}

	for _, m := range found {
		body := m[1]
		if !regexp.MustCompile(`(?is)ORDER BY\s+t\.time\s+DESC`).MatchString(body) {
			t.Errorf("a LATERAL probe takes LIMIT 1 without ORDER BY t.time DESC:\n%s\n\n"+
				"Which row that returns is up to the plan. It looks right today because "+
				"traffic_snapshots is indexed on (ip, time DESC) and the planner uses it; "+
				"without the index the same query returns the older country. The clause is "+
				"the part that makes the answer a property of the query rather than of the "+
				"index.", body)
		}
		if !regexp.MustCompile(`(?is)LIMIT\s+1`).MatchString(body) {
			t.Errorf("a LATERAL probe has no LIMIT 1:\n%s\n\n"+
				"Without it the join multiplies rows per address instead of resolving one "+
				"country for it.", body)
		}
	}
}
