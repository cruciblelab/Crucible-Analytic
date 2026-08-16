package api

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The parameter layout of every query in store_beacon.go is a shared
// convention rather than something the compiler checks:
//
//	$1 site  $2 from  $3 to  $4 bots
//	$5 utm_source  $6 utm_medium  $7 utm_campaign
//	$8 session timeout (only where sessionCTEs is appended)
//	then the query's own limit/offset/interval
//
// Getting a position wrong does not fail loudly. pgx happily binds the
// wrong value to the right slot when the types line up, so the query
// still runs and answers a different question - which is the exact
// failure mode this project has already paid for once (see the
// cumulative-request-count note in NOTES.md: correct arithmetic, wrong
// premise, only a real run caught it).
//
// The behavioural half of this guard lives in
// integration_campaign_test.go, which asserts filtered counts through
// every read method against a live database. This half is the cheap
// structural check that runs without one: it reads the source and
// asserts the convention still holds, so a query added at 2am two years
// from now cannot quietly opt out of it.

const storeBeaconSource = "store_beacon.go"

func readStoreBeacon(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(storeBeaconSource)
	if err != nil {
		t.Fatalf("reading %s: %v", storeBeaconSource, err)
	}
	return string(b)
}

// Every query built on beaconFilterCTE must bind its arguments through
// beaconArgs. Hand-written argument lists are how a position drifts.
func TestBeaconQueries_BindArgumentsThroughBeaconArgs(t *testing.T) {
	src := readStoreBeacon(t)

	// Each pool.Query/QueryRow call, from the call through its argument
	// list, is one statement to check.
	calls := regexp.MustCompile(`(?s)s\.pool\.Query(?:Row)?\(ctx, beaconFilterCTE.*?\n\t\)`).FindAllString(src, -1)
	if len(calls) == 0 {
		t.Fatal("found no beaconFilterCTE queries; this guard has stopped guarding anything")
	}

	for _, call := range calls {
		if !strings.Contains(call, "beaconArgs(") {
			t.Errorf("a beaconFilterCTE query binds its arguments by hand instead of through beaconArgs.\n"+
				"That is how a parameter position drifts silently. Query:\n%s", firstLines(call, 6))
		}
	}
	t.Logf("checked %d beaconFilterCTE queries", len(calls))
}

// beaconArgs supplies exactly seven leading arguments, so $8 is the
// first slot a query owns. A query that references $5, $6 or $7 outside
// the shared CTE has reused a campaign-filter slot for its own value.
func TestBeaconQueries_DoNotReuseTheReservedParameterSlots(t *testing.T) {
	src := readStoreBeacon(t)

	// Drop line comments first. The doc comment above beaconFilterCTE
	// spells the layout out ("$5 utm_source ..."), and documenting the
	// convention must not read as violating it.
	body := stripLineComments(src)

	// Then drop the shared CTE constants: they are where $5-$7
	// legitimately appear in actual SQL.
	for _, shared := range []string{"const beaconFilterCTE = `", "const sessionCTEs = `,"} {
		start := strings.Index(body, shared)
		if start < 0 {
			t.Fatalf("could not find %q; this guard needs updating", shared)
		}
		end := strings.Index(body[start+len(shared):], "`")
		if end < 0 {
			t.Fatalf("unterminated constant after %q", shared)
		}
		body = body[:start] + body[start+len(shared)+end+1:]
	}

	for _, reserved := range []string{"$5", "$6", "$7"} {
		if strings.Contains(body, reserved) {
			t.Errorf("%s is used outside the shared CTEs. Slots $1-$7 belong to beaconArgs "+
				"(site, range, bots, and the three campaign filters); a query's own "+
				"parameters start at $8.", reserved)
		}
	}
}

// sessionCTEs binds the timeout at $8, so any query appending it must
// start its own parameters at $9.
func TestBeaconQueries_SessionQueriesStartTheirOwnParametersAtNine(t *testing.T) {
	src := readStoreBeacon(t)

	calls := regexp.MustCompile(`(?s)s\.pool\.Query(?:Row)?\(ctx, beaconFilterCTE\+sessionCTEs.*?\n\t\)`).FindAllString(src, -1)
	if len(calls) == 0 {
		t.Fatal("found no session queries; this guard has stopped guarding anything")
	}

	for _, call := range calls {
		if !strings.Contains(call, "sessionTimeout") {
			t.Errorf("a query appends sessionCTEs but never binds sessionTimeout:\n%s", firstLines(call, 6))
			continue
		}
		// $8 is the timeout. Anything the query owns must be $9 or above.
		for _, param := range regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(call, -1) {
			n, err := strconv.Atoi(param[1])
			if err != nil {
				continue
			}
			if n > 0 && n < 8 {
				t.Errorf("session query references %s, but $1-$7 belong to beaconArgs "+
					"and $8 to the session timeout:\n%s", param[0], firstLines(call, 6))
				break
			}
		}
	}
	t.Logf("checked %d session queries", len(calls))
}

// The campaign filter is only a filter if every dimension it names is
// actually compared. A CTE that silently stopped applying one would let
// a filtered request return unfiltered numbers.
func TestBeaconFilterCTE_ComparesEveryCampaignDimension(t *testing.T) {
	for _, want := range []string{
		"utm_source = $5",
		"utm_medium = $6",
		"utm_campaign = $7",
	} {
		if !strings.Contains(beaconFilterCTE, want) {
			t.Errorf("beaconFilterCTE no longer contains %q, so that dimension is not being filtered", want)
		}
	}
	// Each comparison must be guarded by an empty check, or an unset
	// filter would exclude every row instead of none.
	for _, want := range []string{"$5::text = ''", "$6::text = ''", "$7::text = ''"} {
		if !strings.Contains(beaconFilterCTE, want) {
			t.Errorf("beaconFilterCTE is missing the %q guard; an unset filter would match nothing", want)
		}
	}
}

// beaconArgs is the single place the leading arguments are built, so its
// count is the contract every query above depends on.
func TestBeaconArgs_SuppliesExactlySevenLeadingArguments(t *testing.T) {
	got := beaconArgs("site", beaconParams{bots: BotsExclude})
	if len(got) != 7 {
		t.Fatalf("beaconArgs returned %d leading arguments, want 7. "+
			"Changing this renumbers every query in store_beacon.go.", len(got))
	}
	withExtras := beaconArgs("site", beaconParams{bots: BotsExclude}, "a", "b")
	if len(withExtras) != 9 {
		t.Errorf("beaconArgs with 2 extras returned %d arguments, want 9", len(withExtras))
	}
}

// stripLineComments removes // comments so that documenting a parameter
// layout is not mistaken for violating it. Crude - it does not know
// about // inside a string literal - and adequate: this package has no
// such literal, and a false positive here is a loud test failure rather
// than a silent wrong answer.
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "\t\t...")
	}
	return strings.Join(lines, "\n")
}
