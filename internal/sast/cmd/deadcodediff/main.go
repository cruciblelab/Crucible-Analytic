// Command deadcodediff compares a deadcode report against the committed
// allowlist and reports only what is new.
//
// It sits beside sastdiff and works the same way, because the failure it
// guards is the same shape as an unexplained gosec finding: something a
// tool noticed, that a person has to decide about, and that must not be
// silenced by being ignored.
//
//	deadcode -test -tags=integration ./... > deadcode.txt
//	go run ./internal/sast/cmd/deadcodediff -report deadcode.txt
//
// Exits non-zero when the report names a function the allowlist does
// not, and also when the allowlist names one the report no longer
// contains - a stale entry is how a future function inherits an
// exemption nobody decided about it.
//
// # Why unreachable code is worth a gate
//
// Code that is never reached cannot fail. Three real defects in this
// repository were exactly that: two retention sweeps and an audit link,
// all correct, all documented, none of them called, and every test green
// the whole time. The tables grew, the column stayed null, and nothing
// anywhere said so.
//
// The allowlist is the half a person maintains, and every entry carries
// a reason. See internal/sast/deadcode_allowlist.txt for what to do when
// this goes red.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

func main() {
	report := flag.String("report", "", "deadcode output to read; - for stdin")
	allowlist := flag.String("allowlist", "internal/sast/deadcode_allowlist.txt", "the committed allowlist")
	flag.Parse()

	if *report == "" {
		fmt.Fprintln(os.Stderr, "deadcodediff: -report is required")
		os.Exit(2)
	}

	found, err := readReport(*report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "deadcodediff: %v\n", err)
		os.Exit(2)
	}
	allowed, err := readAllowlist(*allowlist)
	if err != nil {
		fmt.Fprintf(os.Stderr, "deadcodediff: %v\n", err)
		os.Exit(2)
	}
	if len(allowed) == 0 {
		fmt.Fprintf(os.Stderr, "deadcodediff: %s lists nothing; refusing to run, "+
			"because an empty allowlist makes every comparison below meaningless\n", *allowlist)
		os.Exit(2)
	}

	var unexplained, stale []string
	for name := range found {
		if _, ok := allowed[name]; !ok {
			unexplained = append(unexplained, name)
		}
	}
	for name := range allowed {
		if _, ok := found[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(unexplained)
	sort.Strings(stale)

	if len(unexplained) == 0 && len(stale) == 0 {
		fmt.Printf("no new unreachable code (%d in the report, all explained)\n", len(found))
		return
	}

	if len(unexplained) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d function(s) are unreachable and not explained:\n\n", len(unexplained))
		for _, name := range unexplained {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
		fmt.Fprintf(os.Stderr, `
Code nothing reaches cannot fail, which is why this is a gate rather
than a report: no test will ever go red for it.

Three answers, best first:

  1. Wire it. If it should be called, this has found a bug - which is
     what it found the first three times.
  2. Delete it. A leftover is worse than nothing: somebody reads it and
     assumes it runs.
  3. Add it to %s with the reason, naming the
     phase that will use it.

A name with no reason is not an answer.

`, *allowlist)
	}

	if len(stale) > 0 {
		fmt.Fprintf(os.Stderr, "%d allowlist entr(y/ies) are no longer unreachable:\n\n", len(stale))
		for _, name := range stale {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
		fmt.Fprintf(os.Stderr, `
Remove them from %s. Either somebody wired it up -
in which case the exemption has done its job and should go - or the
function was deleted and its line outlived it. A stale entry is how the
next function of that name inherits a decision nobody made about it.

`, *allowlist)
	}

	os.Exit(1)
}

// reportLine matches deadcode's output:
//
//	internal/panel/audit.go:192:17: unreachable func: Store.Audit
var reportLine = regexp.MustCompile(`^(.+?):\d+:\d+: unreachable func: (.+)$`)

// readReport turns deadcode's output into a set of package-qualified
// names.
//
// Keyed on package plus function rather than on file and line, because a
// line number moves every time somebody edits above it, and an allowlist
// that churned on unrelated edits would be one people stop reading.
func readReport(path string) (map[string]bool, error) {
	f := os.Stdin
	if path != "-" {
		var err error
		f, err = os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
	}

	out := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		m := reportLine.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if m == nil {
			continue
		}
		pkg := m[1]
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			pkg = pkg[:i]
		} else {
			pkg = "."
		}
		out[pkg+"."+m[2]] = true
	}
	return out, scanner.Err()
}

// readAllowlist reads the committed list, ignoring comments and blanks.
//
// An entry with no reason after it is refused rather than accepted: the
// reason is the only thing that stops this file from becoming a place to
// make findings go away.
func readAllowlist(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for n, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, found := strings.Cut(line, "#")
		name = strings.TrimSpace(name)
		reason = strings.TrimSpace(reason)
		if !found || reason == "" {
			return nil, fmt.Errorf("%s:%d: %q has no reason after it; "+
				"an entry without one is how this file stops being read", path, n+1, name)
		}
		out[name] = reason
	}
	return out, nil
}
