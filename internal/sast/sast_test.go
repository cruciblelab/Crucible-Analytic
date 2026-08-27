package sast

import (
	"strings"
	"testing"
)

// The three properties the fingerprint has to have. They are not three
// tests of one function so much as the three reasons this package exists
// instead of gosec's --exclude-rules, so each one names the failure it
// prevents.

// TestFingerprint_SurvivesALineShift: an edit somewhere above a finding
// moves it down the file. Nothing about the finding changed.
//
// A baseline that goes off every time an unrelated line is added above is
// a baseline whose first false alarm teaches everyone to regenerate it
// without reading it - at which point it silences real findings by
// design.
func TestFingerprint_SurvivesALineShift(t *testing.T) {
	before := Issue{
		RuleID: "G304", File: "internal/logging/tree.go", Line: "254",
		Code: "253: \tpath := filepath.Join(dir, name)\n254: \tf, err := os.OpenFile(path, flags, 0o644)\n255: \tif err != nil {\n",
	}
	// Same code, ten lines further down, and gosec renumbers its excerpt.
	after := Issue{
		RuleID: "G304", File: "internal/logging/tree.go", Line: "264",
		Code: "263: \tpath := filepath.Join(dir, name)\n264: \tf, err := os.OpenFile(path, flags, 0o644)\n265: \tif err != nil {\n",
	}

	if Fingerprint(before) != Fingerprint(after) {
		t.Errorf("the same code at a different line produced a different fingerprint:\n  %s\n  %s",
			Fingerprint(before), Fingerprint(after))
	}
}

// TestFingerprint_ChangesWhenTheFlaggedCodeChanges: the line that was
// triaged is now different code.
//
// This is the property that keeps the baseline honest. "This conversion
// is bounded" was a judgement about particular code; once that code
// changes, the judgement has not been made about what is there now. So
// the finding comes back and somebody looks again.
func TestFingerprint_ChangesWhenTheFlaggedCodeChanges(t *testing.T) {
	triaged := Issue{
		RuleID: "G115", File: "internal/storage/row.go", Line: "128",
		Code: "127: \t\t\tRequestRate: snap.EstimatedRate,\n128: \t\t\tBotScore: int16(result.Score),\n129: \t\t\tIsKnownBotJA4: result.IsKnownBotJA4,\n",
	}
	// The clamp that made it safe is gone. Same rule, same file, same
	// line number - and it must not still match.
	edited := Issue{
		RuleID: "G115", File: "internal/storage/row.go", Line: "128",
		Code: "127: \t\t\tRequestRate: snap.EstimatedRate,\n128: \t\t\tBotScore: int16(result.RawScore),\n129: \t\t\tIsKnownBotJA4: result.IsKnownBotJA4,\n",
	}

	if Fingerprint(triaged) == Fingerprint(edited) {
		t.Error("editing the flagged line did not change its fingerprint; a triage decision would carry over to code it was never made about")
	}
}

// TestFingerprint_DistinguishesANewFindingInTheSameFile is the case that
// made this package necessary.
//
// gosec's own --exclude-rules="file:RULE" was measured doing the wrong
// thing here: with G402 suppressed for a file, a second, genuinely broken
// G402 added to that same file reported nothing (2 findings became 0).
// The email wizard will add exactly this shape to auth.go, which already
// carries three baselined findings.
func TestFingerprint_DistinguishesANewFindingInTheSameFile(t *testing.T) {
	triaged := Issue{
		RuleID: "G402", File: "internal/mail/smtp.go", Line: "12",
		Code: "11: func dial(host string) (*tls.Conn, error) {\n12: \treturn tls.Dial(\"tcp\", host, tlsConfigFor(host))\n13: }\n",
	}
	// Somebody adds a second dialler and turns verification off to make
	// it work against a relay with a self-signed certificate.
	added := Issue{
		RuleID: "G402", File: "internal/mail/smtp.go", Line: "20",
		Code: "19: func dialRelay(host string) (*tls.Conn, error) {\n20: \treturn tls.Dial(\"tcp\", host, &tls.Config{InsecureSkipVerify: true})\n21: }\n",
	}

	base := &Baseline{Entries: []Entry{{
		Fingerprint: Fingerprint(triaged), Rule: "G402",
		File: "internal/mail/smtp.go", Reason: "verification is on; config comes from tlsConfigFor",
	}}}
	rep := &Report{Issues: []Issue{triaged, added}}

	res := Compare(rep, base)
	if len(res.New) != 1 {
		t.Fatalf("Compare reported %d new findings, want 1: the suppression swallowed a real one, which is the whole reason gosec's own mechanism was rejected", len(res.New))
	}
	if res.New[0].Line != "20" {
		t.Errorf("reported the wrong finding as new: line %s", res.New[0].Line)
	}
	if len(res.Stale) != 0 {
		t.Errorf("the baselined finding was reported stale even though it is still present: %+v", res.Stale)
	}
}

// TestCompare_ReportsAStaleEntry: a finding was fixed, and its baseline
// line is still there.
//
// Left unreported, the baseline slowly stops describing the code and
// starts describing its own history, and every dead line is one more
// place nobody is looking.
func TestCompare_ReportsAStaleEntry(t *testing.T) {
	fixed := Entry{
		Fingerprint: "deadbeefdeadbeefdeadbeef", Rule: "G112",
		File: "internal/fullproxy/server.go", Reason: "no longer true, timeouts were added",
	}
	base := &Baseline{Entries: []Entry{fixed}}

	res := Compare(&Report{}, base)
	if len(res.Stale) != 1 {
		t.Fatalf("Compare reported %d stale entries, want 1", len(res.Stale))
	}
	if res.Stale[0].Rule != "G112" {
		t.Errorf("stale entry = %s, want G112", res.Stale[0].Rule)
	}
}

// TestFingerprint_IgnoresPureWhitespaceChanges: gofmt wrapping a long
// argument list is not a change to re-triage.
func TestFingerprint_IgnoresPureWhitespaceChanges(t *testing.T) {
	tight := Issue{
		RuleID: "G204", File: "internal/install/run.go", Line: "8",
		Code: "7: func run(c string) error {\n8: \treturn exec.Command(\"sh\", \"-c\", c).Run()\n9: }\n",
	}
	wrapped := Issue{
		RuleID: "G204", File: "internal/install/run.go", Line: "8",
		Code: "7: func run(c string) error {\n8: \t\t\treturn exec.Command(\"sh\", \"-c\", c).Run()   \n9: }\n",
	}

	if Fingerprint(tight) != Fingerprint(wrapped) {
		t.Error("reindenting the flagged line changed its fingerprint")
	}
}

// TestFingerprint_SeparatesTheSameIdiomInDifferentFiles: two files with
// the identical flagged line were triaged separately, and either can stop
// being true on its own.
func TestFingerprint_SeparatesTheSameIdiomInDifferentFiles(t *testing.T) {
	code := "9: \tf, err := os.Open(path)\n"
	a := Issue{RuleID: "G304", File: "internal/logging/tree.go", Line: "9", Code: code}
	b := Issue{RuleID: "G304", File: "internal/botdata/botdata.go", Line: "9", Code: code}

	if Fingerprint(a) == Fingerprint(b) {
		t.Error("the same idiom in two files shares one fingerprint; triaging one would silence the other")
	}
}

func TestParseReport_ReadsGosecJSON(t *testing.T) {
	const raw = `{"Issues":[{"rule_id":"G402","details":"TLS InsecureSkipVerify set to true.","file":"/repo/internal/mail/smtp.go","line":"12","confidence":"HIGH","severity":"HIGH","code":"12: \tx := 1\n"}],"Stats":{"files":1}}`

	rep, err := ParseReport(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(rep.Issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(rep.Issues))
	}
	if got := rep.Issues[0].RuleID; got != "G402" {
		t.Errorf("rule_id = %q, want G402", got)
	}
	if got := rep.Issues[0].Severity; got != "HIGH" {
		t.Errorf("severity = %q, want HIGH", got)
	}
}

// TestFlaggedLine_PicksTheLineGosecPointedAt - the Snippet field is for a
// human reading the baseline, so it has to be the line that was actually
// flagged rather than the first line of the excerpt.
func TestFlaggedLine_PicksTheLineGosecPointedAt(t *testing.T) {
	i := Issue{
		RuleID: "G115", File: "x.go", Line: "128",
		Code: "127: \t\tRequestRate: r,\n128: \t\tBotScore: int16(s),\n129: \t\tCountry: c,\n",
	}
	if got, want := flaggedLine(i), "BotScore: int16(s),"; got != want {
		t.Errorf("flaggedLine() = %q, want %q", got, want)
	}
}

// TestRelativize_MakesTheBaselinePortable.
//
// gosec reports absolute paths and the fingerprint includes the path, so
// a baseline written in /home/user/Crucible-Analytic matches nothing in
// /home/runner/work/... This is the failure that would have shipped
// quietly: every finding new on every CI run, and the obvious "fix" -
// regenerating the baseline on the runner - breaks it locally instead.
func TestRelativize_MakesTheBaselinePortable(t *testing.T) {
	const code = "12: \tf, err := os.Open(path)\n"

	dev := &Report{Issues: []Issue{{
		RuleID: "G304", File: "/home/user/Crucible-Analytic/internal/logging/tree.go", Line: "12", Code: code,
	}}}
	ci := &Report{Issues: []Issue{{
		RuleID: "G304", File: "/home/runner/work/repo/repo/internal/logging/tree.go", Line: "12", Code: code,
	}}}

	// Without normalisation the two must genuinely differ, or this test
	// proves nothing about what Relativize fixes.
	if Fingerprint(dev.Issues[0]) == Fingerprint(ci.Issues[0]) {
		t.Fatal("two different absolute paths already agreed; this test cannot show what Relativize is for")
	}

	dev.Relativize("/home/user/Crucible-Analytic")
	ci.Relativize("/home/runner/work/repo/repo")

	if dev.Issues[0].File != "internal/logging/tree.go" {
		t.Errorf("dev path = %q, want internal/logging/tree.go", dev.Issues[0].File)
	}
	if got, want := Fingerprint(dev.Issues[0]), Fingerprint(ci.Issues[0]); got != want {
		t.Errorf("after Relativize the fingerprints still differ:\n  dev %s\n  ci  %s", got, want)
	}
}

// TestMerge_KeepsTheReasonsAlreadyWritten is the whole reason Merge
// exists. The alternative on offer before it was -init, which rewrites
// the file and empties every reason - twenty-one paragraphs of triage
// gone to add one line.
func TestMerge_KeepsTheReasonsAlreadyWritten(t *testing.T) {
	triaged := Issue{
		RuleID: "G115", File: "internal/beacon/writer.go", Line: "40",
		Code: "40: \tw.written.Add(uint64(n))\n",
	}
	base := &Baseline{
		Note: "keep me",
		Entries: []Entry{{
			Fingerprint: Fingerprint(triaged),
			Rule:        triaged.RuleID,
			File:        triaged.File,
			Reason:      "n is the row count CopyFrom returned on success",
			Snippet:     "w.written.Add(uint64(n))",
		}},
	}

	fresh := Issue{
		RuleID: "G101", File: "internal/mail/diagnose.go", Line: "51",
		Severity: "HIGH",
		Code:     "51: \tDiagBadCredentials Diagnosis = \"kimlik_reddedildi\"\n",
	}

	merged, added := Merge(base, &Report{Issues: []Issue{triaged, fresh}})

	if len(added) != 1 || added[0].Rule != "G101" {
		t.Fatalf("added = %+v, want the one new G101", added)
	}
	if len(merged.Entries) != 2 {
		t.Fatalf("merged baseline has %d entries, want 2", len(merged.Entries))
	}
	if merged.Note != "keep me" {
		t.Errorf("note = %q, want the existing one", merged.Note)
	}

	byRule := make(map[string]Entry, len(merged.Entries))
	for _, e := range merged.Entries {
		byRule[e.Rule] = e
	}
	if got := byRule["G115"].Reason; got != "n is the row count CopyFrom returned on success" {
		t.Errorf("the existing reason became %q", got)
	}
	// The new one arrives empty, so the reason has to be written by a
	// person rather than invented by the tool. TestBaseline_EveryEntryHasAReason
	// is what refuses the file until it is.
	if byRule["G101"].Reason != "" {
		t.Errorf("the new entry arrived with a reason nobody wrote: %q", byRule["G101"].Reason)
	}
	if byRule["G101"].Snippet == "" {
		t.Error("the new entry has no snippet for a reader to judge")
	}
}

// Running Merge twice must not double the file. The obvious way to get
// this wrong is to key on the report rather than on what is already
// there, and the result would be a baseline that grows every time
// somebody runs the command.
func TestMerge_IsIdempotent(t *testing.T) {
	issue := Issue{
		RuleID: "G101", File: "internal/mail/diagnose.go", Line: "51",
		Code: "51: \tDiagBadCredentials Diagnosis = \"kimlik_reddedildi\"\n",
	}
	rep := &Report{Issues: []Issue{issue}}

	once, added := Merge(&Baseline{}, rep)
	if len(added) != 1 {
		t.Fatalf("first merge added %d, want 1", len(added))
	}
	twice, addedAgain := Merge(once, rep)
	if len(addedAgain) != 0 {
		t.Errorf("second merge added %d more, want 0", len(addedAgain))
	}
	if len(twice.Entries) != 1 {
		t.Errorf("baseline has %d entries after merging the same report twice, want 1", len(twice.Entries))
	}
}

// A report containing two findings that hash the same must not produce
// two entries - the second would be a line nothing can ever match, which
// is the stale-entry problem created at birth.
func TestMerge_DoesNotDuplicateWithinOneReport(t *testing.T) {
	issue := Issue{
		RuleID: "G304", File: "internal/logging/tree.go", Line: "9",
		Code: "9: \tf, err := os.Open(path)\n",
	}
	merged, added := Merge(&Baseline{}, &Report{Issues: []Issue{issue, issue}})
	if len(added) != 1 || len(merged.Entries) != 1 {
		t.Errorf("added %d entries for the same finding twice, want 1", len(merged.Entries))
	}
}
