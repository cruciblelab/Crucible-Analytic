package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryUnreadableMessageComesFromTheOnePlaceThatLogsIt.
//
// The rule this holds is structural rather than behavioural, because the
// failure it guards against is a *new* page: somebody adds a section,
// copies the two lines that turn an error into "Okunamadı.", and the
// error goes on the floor again. No behavioural test can fail for a
// page nobody has written yet.
//
// So the message keys are the invariant. Each appears in exactly one
// place in this package - internal/panel/web/unreadable.go - and that
// place logs. A page that wants the sentence has to go through the call
// that writes the line.
//
// # Why it reads the source instead of the catalogue
//
// The catalogue would say the keys exist. What matters is who writes
// them, and only the source says that.
func TestEveryUnreadableMessageComesFromTheOnePlaceThatLogsIt(t *testing.T) {
	// The two keys a failed analytics call can produce. Not derived from
	// the message catalogue on purpose: this list is about which
	// sentences must be accompanied by a log line, which is a smaller
	// and different set from "every message about emptiness".
	keys := []string{"pano.hata.ulasilamiyor", "pano.hata.reddedildi"}

	// The one file allowed to write them.
	const allowed = "unreadable.go"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	offenders := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, key := range keys {
			if !strings.Contains(string(body), `"`+key+`"`) {
				continue
			}
			if name == allowed {
				continue
			}
			offenders[name] = append(offenders[name], key)
		}
	}

	// The scan has to have looked at something, or an empty directory
	// listing would report a clean package.
	if scanned < 5 {
		t.Fatalf("only %d non-test .go files scanned, which is too few to be this "+
			"package - this test would pass on a directory it could not read", scanned)
	}
	// And the allowed file has to actually contain them, or the rule is
	// being kept by the keys having been renamed out from under it.
	body, err := os.ReadFile(allowed)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if !strings.Contains(string(body), `"`+key+`"`) {
			t.Errorf("%s does not mention %q. Either the key was renamed and this "+
				"test now guards nothing, or the sentence is being produced "+
				"somewhere else again", allowed, key)
		}
	}

	for file, found := range offenders {
		t.Errorf("%s writes %v itself.\n"+
			"That sentence tells a reader a number could not be fetched, and it "+
			"has to be accompanied by a line telling the operator why - which is "+
			"what %s does and what three separate copies of these two lines used "+
			"to skip. Call s.unreadable instead; it returns the same string",
			file, found, allowed)
	}
}

// TestTheUnreadableLogLineNamesWhatAnOperatorWouldAskFor.
//
// A line that says only "a section could not be read" is the page's
// sentence again, in a file nobody reads. The point of logging is the
// four things the page cannot show: which section, which site, which
// range, and the error.
//
// Held against the source of the one function, because the alternative -
// capturing a real log line - needs a failing analytics call, and this
// assertion is about the shape of the line rather than about the failure
// that produced it. The behaviour is covered separately, against a
// server whose API is not there.
func TestTheUnreadableLogLineNamesWhatAnOperatorWouldAskFor(t *testing.T) {
	body, err := os.ReadFile("unreadable.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	// The Warn call and only it, so that a field belonging to some other
	// call in this file cannot satisfy the check. Delimited by matching
	// parentheses rather than by a blank line: the call's last argument
	// and the function's closing brace are adjacent, and a regex anchored
	// on layout would fail the day somebody reformats.
	call, ok := callArgs(src, "s.logger().Warn(")
	if !ok {
		t.Fatal("no s.logger().Warn(...) call found in unreadable.go, so nothing " +
			"here is checking a log line at all")
	}

	for _, want := range []struct{ field, why string }{
		{`"where"`, "which section failed. Without it an operator knows the page " +
			"was short and not which half of it"},
		{`"site"`, "which site. A deployment has many, and a fault on one of them " +
			"is a different problem from a fault on all"},
		{`"span"`, "how long a range was asked for. This is the field that " +
			"separates a service that is down from a query that is too slow, " +
			"and the page looks identical either way"},
		{`"err"`, "the error itself, which is the whole reason this exists"},
	} {
		if !strings.Contains(call, want.field) {
			t.Errorf("the log line does not carry %s: %s", want.field, want.why)
		}
	}
}

// callArgs returns the argument text of the first call to name in src,
// delimited by its own matching parentheses.
func callArgs(src, name string) (string, bool) {
	i := strings.Index(src, name)
	if i < 0 {
		return "", false
	}
	depth := 0
	start := i + len(name)
	for j := start - 1; j < len(src); j++ {
		switch src[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[start:j], true
			}
		}
	}
	return "", false
}
