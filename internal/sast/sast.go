// Package sast compares a gosec report against a committed baseline of
// findings that have already been triaged, so a scan reports only what is
// new.
//
// # Why this exists rather than gosec's own suppression
//
// gosec's only built-in mechanism is --exclude-rules="path:RULE", which
// suppresses a rule for a whole file. That was measured before this
// package was written, not assumed: a rule was suppressed for a file, a
// genuinely new finding of the same rule was then added to that same
// file, and the scanner reported nothing.
//
//	suppression off:  2 findings
//	suppression on:   0 findings
//
// So the mechanism hides exactly what a baseline exists to surface. And
// it does it in the worst place: internal/panel/web/auth.go carries three
// findings today and is a file the email wizard (C7.3) and the dashboard
// group will both grow.
//
// # What is keyed instead
//
// A finding's identity here is the rule, the file, and a hash of the
// flagged source itself - never the line number. That is the only keying
// that gets all three cases right:
//
//   - lines shift because something above them changed: the flagged code
//     is unchanged, so the fingerprint is unchanged, so the scan stays
//     quiet. A baseline that goes off on every unrelated edit is a
//     baseline someone deletes.
//   - the flagged line itself is edited: the fingerprint changes and the
//     finding comes back for triage. This is correct rather than
//     annoying - "this conversion is bounded" was a judgement about
//     particular code, and that code is now different code.
//   - a new finding of the same rule appears in the same file: it has its
//     own fingerprint, so it is reported. This is the case gosec's own
//     mechanism cannot express.
package sast

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Report is the subset of gosec's JSON output this package reads.
type Report struct {
	Issues []Issue `json:"Issues"`
}

// Issue is one gosec finding. The field names are gosec's own.
type Issue struct {
	RuleID     string `json:"rule_id"`
	Details    string `json:"details"`
	File       string `json:"file"`
	Line       string `json:"line"`
	Confidence string `json:"confidence"`
	Severity   string `json:"severity"`
	Code       string `json:"code"`
}

// Entry is one triaged finding in the baseline file.
type Entry struct {
	Fingerprint string `json:"fingerprint"`
	Rule        string `json:"rule"`
	File        string `json:"file"`
	Severity    string `json:"severity"`
	// Reason is why this finding is not a defect. Required: a baseline
	// entry without one is indistinguishable from a finding somebody
	// silenced because it was noisy on a Friday. The test suite refuses
	// a baseline with an empty reason.
	Reason string `json:"reason"`
	// Snippet is the flagged line as it was when triaged, stored for
	// humans reading the baseline. It is not what the fingerprint is
	// computed from at compare time - the report's own code is - so
	// editing this field cannot silence anything.
	Snippet string `json:"snippet"`
}

// Baseline is the committed set of already-triaged findings.
type Baseline struct {
	Note    string  `json:"note"`
	Entries []Entry `json:"entries"`
}

// linePrefix matches the "12: " that gosec puts at the start of every
// line in its code excerpt. Stripping it is what makes a fingerprint
// survive a line shift: the same code at a different line number
// produces the same hash.
var linePrefix = regexp.MustCompile(`(?m)^\s*\d+:\s?`)

// Fingerprint identifies a finding by what was flagged rather than by
// where it was. See the package comment for the three cases this keying
// has to get right.
//
// The file path is included so the same idiom flagged in two files stays
// two findings - they were triaged separately and either could stop being
// true on its own. The line number is excluded for the same reason it is
// excluded everywhere else here.
func Fingerprint(i Issue) string {
	code := linePrefix.ReplaceAllString(i.Code, "")

	// Whitespace-only differences are not changes worth re-triaging:
	// gofmt moving an argument onto its own line does not make a
	// bounded integer conversion unbounded.
	var b strings.Builder
	for _, line := range strings.Split(code, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	sum := sha256.Sum256([]byte(i.RuleID + "\x00" + i.File + "\x00" + b.String()))
	return hex.EncodeToString(sum[:12])
}

// ParseReport reads gosec's JSON output.
func ParseReport(r io.Reader) (*Report, error) {
	var rep Report
	if err := json.NewDecoder(r).Decode(&rep); err != nil {
		return nil, fmt.Errorf("sast: parsing gosec report: %w", err)
	}
	return &rep, nil
}

// Relativize rewrites absolute file paths to be relative to root.
//
// gosec reports absolute paths, and the fingerprint includes the path -
// so without this, a baseline written on a development machine matches
// nothing on a CI runner that checked the repository out somewhere else.
// Every finding would look new, every night, and the first person to see
// that would regenerate the baseline on the runner and break it locally
// instead. Silent, symmetrical, and permanent.
func (rep *Report) Relativize(root string) {
	root = strings.TrimSuffix(root, "/") + "/"
	for i := range rep.Issues {
		rep.Issues[i].File = strings.TrimPrefix(rep.Issues[i].File, root)
	}
}

// ParseBaseline reads the committed baseline file.
func ParseBaseline(r io.Reader) (*Baseline, error) {
	var b Baseline
	if err := json.NewDecoder(r).Decode(&b); err != nil {
		return nil, fmt.Errorf("sast: parsing baseline: %w", err)
	}
	return &b, nil
}

// Result is what a comparison found.
type Result struct {
	// New are findings not in the baseline. These are what a scan
	// exists to surface.
	New []Issue
	// Stale are baseline entries that matched nothing in the report,
	// meaning the code they described is gone or fixed.
	//
	// Reported rather than ignored, because a baseline that only ever
	// grows stops describing the code and starts describing its own
	// history - and every stale line is one more place a future finding
	// could hide behind a fingerprint collision nobody would notice.
	Stale []Entry
}

// Compare reports what changed between the baseline and a fresh report.
func Compare(rep *Report, base *Baseline) Result {
	inBaseline := make(map[string]Entry, len(base.Entries))
	for _, e := range base.Entries {
		inBaseline[e.Fingerprint] = e
	}

	matched := make(map[string]bool, len(base.Entries))
	var res Result

	for _, issue := range rep.Issues {
		fp := Fingerprint(issue)
		if _, ok := inBaseline[fp]; ok {
			matched[fp] = true
			continue
		}
		res.New = append(res.New, issue)
	}

	for _, e := range base.Entries {
		if !matched[e.Fingerprint] {
			res.Stale = append(res.Stale, e)
		}
	}

	sort.Slice(res.New, func(a, b int) bool {
		if res.New[a].File != res.New[b].File {
			return res.New[a].File < res.New[b].File
		}
		return res.New[a].Line < res.New[b].Line
	})
	return res
}

// BaselineFrom builds a baseline from a report, leaving every Reason
// empty. Used to bootstrap the file; the reasons are then written by a
// person, and the test suite refuses the file until they are.
func BaselineFrom(rep *Report) *Baseline {
	b := &Baseline{Entries: make([]Entry, 0, len(rep.Issues))}
	for _, i := range rep.Issues {
		b.Entries = append(b.Entries, Entry{
			Fingerprint: Fingerprint(i),
			Rule:        i.RuleID,
			File:        i.File,
			Severity:    i.Severity,
			Snippet:     flaggedLine(i),
		})
	}
	sort.Slice(b.Entries, func(x, y int) bool {
		if b.Entries[x].File != b.Entries[y].File {
			return b.Entries[x].File < b.Entries[y].File
		}
		return b.Entries[x].Rule < b.Entries[y].Rule
	})
	return b
}

// flaggedLine pulls the line gosec pointed at out of its excerpt, for the
// human-readable Snippet field.
func flaggedLine(i Issue) string {
	for _, line := range strings.Split(i.Code, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), i.Line+":") {
			return strings.TrimSpace(linePrefix.ReplaceAllString(line, ""))
		}
	}
	return ""
}
