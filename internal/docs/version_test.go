package docs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
)

// The release notes are read by exactly one kind of person: somebody
// deciding whether to upgrade, and what they will have to do afterwards.
// Two things in them can go quietly wrong, and both are worse than a
// missing note because a wrong note is believed.

// changelogHeading matches "## v0.9.0+L3 — 2026-08-31".
//
// The version part is matched strictly rather than as ".*", so a heading
// that is not a version - a section somebody added - is not read as one.
// The strictness is the check: see TestTheChangelogVersionIsSemVer.
var changelogHeading = regexp.MustCompile(`(?m)^## (v[^ ]+)`)

// semver is the version grammar this project uses, and the reason it is
// written out here rather than pulled in as a dependency: what has to
// hold is that the string parses as SemVer *and* carries the phase in
// build metadata, which no off-the-shelf parser asserts.
var semver = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:\+([0-9A-Za-z.-]+))?$`)

// unreleasedHeading is the section work lands in before it is tagged.
//
// # Why this exists, and what happened without it
//
// The rule below - the newest entry names the schema the code carries -
// was written when the newest entry was always the one being worked on.
// After a tag it stops being true: v0.20.0 shipped with schema 8, work
// continued, the schema became 9, and the only way to satisfy a check
// that looked at the newest *version* heading was to go back and edit
// v0.20.0's note.
//
// Which is what happened. The v0.20.0 entry was rewritten in place to
// say "Şema sürümü: 9" and to tell the operator to run Sağlık → Şema
// yükseltmesi - for a release that carries neither. Anybody reading the
// notes for the version they actually installed was told to perform an
// upgrade that release does not contain.
//
// The test's own comment said older entries "must not be rewritten". It
// said so while making that the only way to pass.
//
// *Bir kurala uymanın meşru bir yolu yoksa, o kural insanlara kuralı
// çiğnemeyi öğretir.* So the file gets a heading that new work belongs
// under, and the check below looks at whichever entry is first.
var unreleasedHeading = regexp.MustCompile(`(?m)^## (Yayımlanmamış|Unreleased)\s*$`)

// TestTheChangelogVersionIsSemVer.
//
// A version string is read by machines as well as people: `git describe`
// puts it into every binary, release/build.sh names the package after
// it, and Go's module tooling and `sort -V` both assume the grammar.
//
// The two forms VERSIONING.md rejects are the two somebody would
// naturally reach for - "v0.9.0L3", which no parser accepts, and
// "v0.9.0-L3", which parses as a *pre-release* and therefore sorts
// before v0.9.0. Neither fails loudly at the moment it is written.
func TestTheChangelogVersionIsSemVer(t *testing.T) {
	body := readDoc(t, "CHANGELOG.md")

	found := changelogHeading.FindAllStringSubmatch(body, -1)
	if len(found) == 0 {
		t.Fatal("CHANGELOG.md has no version headings, so every check below " +
			"would pass by looking at nothing")
	}

	for _, m := range found {
		version := m[1]
		parts := semver.FindStringSubmatch(version)
		if parts == nil {
			t.Errorf("%q is not a version this project's tooling can read.\n"+
				"VERSIONING.md has the grammar: vMAJOR.MINOR.PATCH+PHASE. "+
				"In particular a hyphen means pre-release in SemVer, which sorts "+
				"*below* the same version without it", version)
			continue
		}
		if strings.Contains(version, "-") {
			t.Errorf("%q carries a hyphen, which SemVer reads as a pre-release: it "+
				"would sort below the plain version. The phase goes after '+', "+
				"which is build metadata and is ignored for ordering", version)
		}
	}
}

// TestEveryReleasedPhaseCodeIsARealPhase.
//
// The phase in a version is a claim: "this is the build where L3
// landed". PLAN.md is where phases are defined, so the claim is checked
// against it.
//
// Worth a test because the failure is silent in the direction that
// matters. A typo - +L4 for +L3, +M1 before M1 exists - produces a
// version string that every tool accepts, sits in the release package's
// name and in every binary, and is wrong in a way only somebody who
// knows the plan by heart would notice.
func TestEveryReleasedPhaseCodeIsARealPhase(t *testing.T) {
	plan := readDoc(t, "PLAN.md")

	// plan_test.go's phaseHeading, not a second copy of it. Two regexps
	// for one heading format is two things to keep in step, and the one
	// that drifts is the one nobody is looking at.
	real := map[string]bool{}
	for _, line := range strings.Split(plan, "\n") {
		if m := phaseHeading.FindStringSubmatch(line); m != nil {
			real[m[1]] = true
		}
	}
	if len(real) == 0 {
		t.Fatal("no phase headings found in PLAN.md; this test would accept any " +
			"phase code at all")
	}

	for _, m := range changelogHeading.FindAllStringSubmatch(readDoc(t, "CHANGELOG.md"), -1) {
		parts := semver.FindStringSubmatch(m[1])
		if parts == nil || parts[4] == "" {
			continue // not a version, or carries no phase; the test above covers that
		}
		phase := parts[4]
		if !real[phase] {
			t.Errorf("%s says it is the %s release and PLAN.md has no phase %s.\n"+
				"Phases in the plan: %s", m[1], phase, phase, strings.Join(sortedKeys(real), " "))
		}
	}
}

// TestTheChangelogNamesTheSchemaVersionTheCodeCarries.
//
// The one number in a release note an operator acts on. It decides
// whether they are about to be asked to upgrade the database, and this
// project built three phases of machinery (L1, L2, L3) around it.
//
// The failure is the quiet kind: somebody adds a schema file, bumps
// schemaver.Version, and the note still says the old number. The
// operator reads "schema 3", sees their database on 3, concludes there
// is nothing to do - and the binary refuses to start, because L2 stops a
// service whose schema is behind rather than letting it lose rows.
func TestTheChangelogNamesTheSchemaVersionTheCodeCarries(t *testing.T) {
	body := readDoc(t, "CHANGELOG.md")

	// The newest entry only, and "newest" includes the unreleased
	// section: after a tag, the work that follows it is what carries the
	// current schema, and a released note describes the release rather
	// than the tip. Older entries name the schema *they* shipped with,
	// which is the point of a changelog and must not be rewritten.
	newest, ok := newestEntry(body)
	if !ok {
		t.Fatal("CHANGELOG.md has no version headings")
	}

	stated := regexp.MustCompile(`[Şş]ema sürümü:?\*{0,2} *(\d+)`).FindStringSubmatch(newest)
	if stated == nil {
		t.Fatalf("the newest CHANGELOG.md entry does not name a schema version.\n"+
			"It is the number an operator acts on: it decides whether they are about "+
			"to be asked to upgrade the database. This build carries %d",
			schemaver.Version)
	}
	got, err := strconv.Atoi(stated[1])
	if err != nil {
		t.Fatalf("unreadable schema version %q in CHANGELOG.md", stated[1])
	}
	if got != schemaver.Version {
		t.Errorf("CHANGELOG.md's newest entry says schema %d and "+
			"internal/schemaver.Version is %d.\n"+
			"An operator reading the note would conclude there is nothing to do, and "+
			"L2 would then refuse to start the service rather than let it write into "+
			"a schema that has no column for it", got, schemaver.Version)
	}
}

// newestEntry returns the topmost entry: the unreleased section when
// there is one, otherwise the newest version.
//
// Bounded by whichever heading comes next, of either kind, so an
// unreleased section is not read as running into the release below it.
func newestEntry(body string) (string, bool) {
	start := -1
	if idx := unreleasedHeading.FindStringIndex(body); idx != nil {
		start = idx[0]
	}
	if idx := changelogHeading.FindStringIndex(body); idx != nil {
		if start < 0 || idx[0] < start {
			start = idx[0]
		}
	}
	if start < 0 {
		return "", false
	}
	entry := body[start:]
	// From just past this heading, so the search does not find it again.
	end := len(entry)
	for _, re := range []*regexp.Regexp{changelogHeading, unreleasedHeading} {
		if idx := re.FindStringIndex(entry[2:]); idx != nil && idx[0]+2 < end {
			end = idx[0] + 2
		}
	}
	return entry[:end], true
}

// TestTheUnreleasedSectionIsSingularAndFirst.
//
// Two of them, or one below a release, and "the newest entry" stops
// meaning anything - which is the state the check above silently
// tolerated before this file knew the heading existed.
func TestTheUnreleasedSectionIsSingularAndFirst(t *testing.T) {
	body := readDoc(t, "CHANGELOG.md")

	found := unreleasedHeading.FindAllStringIndex(body, -1)
	if len(found) == 0 {
		// Legitimate: a tree whose tip is exactly a release has nothing
		// unreleased in it.
		return
	}
	if len(found) > 1 {
		t.Errorf("CHANGELOG.md has %d unreleased sections. Work has one place to "+
			"go, or it goes into a released note", len(found))
	}
	if version := changelogHeading.FindStringIndex(body); version != nil && version[0] < found[0][0] {
		t.Error("CHANGELOG.md's unreleased section sits below a released version. " +
			"It is the newest thing in the file by definition, and a reader takes " +
			"the top entry as current")
	}
}

// sortedKeys, for a message that lists what was actually found.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Sorted so the failure message is stable between runs; an error
	// whose text reshuffles is one people stop reading.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// readDoc reads one repository-root document.
func readDoc(t *testing.T, name string) string {
	t.Helper()
	files := markdownFiles(t)
	body, ok := files[name]
	if !ok {
		t.Fatal(fmt.Sprintf("%s is missing from the repository root", name))
	}
	return body
}

// TestEveryReleaseNoteHasATagAndEveryTagHasANote.
//
// The two sides of a release record, and a gap in either one is quiet.
//
// A note with no tag is a version nobody can install: it reads as
// released, and `git checkout v0.11.1` says the ref does not exist. A tag
// with no note is worse in the other direction - somebody installs it and
// has no way to find out whether they have to do anything, which is the
// one question CHANGELOG.md exists to answer.
//
// # Where this came from
//
// v0.11.0+M1's note described a fix that landed one commit *after* the
// tag. The prose was right about the work and wrong about the version,
// and nothing anywhere could have said so: the note and the tag were
// written in the same hour by the same person and never compared.
//
// This does not catch that exact mistake - no test reads prose - but it
// catches its whole family, which is a release record and a set of refs
// drifting apart while both look complete on their own.
//
// The message below names three causes because the first version named
// one, and the one it named was the wrong one the first time this test
// went red for real. v0.13.0+M3 was tagged and pushed from a different
// clone; this one had the five older tags and had never fetched the new
// one, so the check was right that the tag was missing *here* and its
// advice - cut it - would have created a second tag object for a version
// that already had one. The zero-tag skip above cannot cover this: a
// clone that is merely behind still has tags, just not all of them.
func TestEveryReleaseNoteHasATagAndEveryTagHasANote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; this test compares the changelog against the tags")
	}
	root := repoRoot(t)

	out, err := exec.Command("git", "-C", root, "tag", "--list", "v*").Output()
	if err != nil {
		t.Skipf("git tag failed: %v", err)
	}
	tags := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tags[line] = true
		}
	}
	if len(tags) == 0 {
		// A checkout without tags cannot answer this, and saying so is not
		// the same as passing. actions/checkout fetches none at depth 1,
		// and a contributor who cloned with --depth would meet a mystery
		// instead of a skip - the same shape the CLA test met and wrote
		// down.
		t.Skip("this checkout has no tags, so any answer here would be about the " +
			"checkout rather than the repository. Fetch them (git fetch --tags) to " +
			"run this check")
	}

	var noted []string
	for _, m := range changelogHeading.FindAllStringSubmatch(readDoc(t, "CHANGELOG.md"), -1) {
		if semver.MatchString(m[1]) {
			noted = append(noted, m[1])
		}
	}
	if len(noted) == 0 {
		t.Fatal("CHANGELOG.md names no versions; both directions below would pass " +
			"by comparing against nothing")
	}

	// The newest entry may legitimately have no tag yet: VERSIONING.md's
	// own procedure writes the note (step 2) before cutting the tag
	// (step 3), so requiring one here would make following the written
	// order fail the gate.
	for _, version := range noted[1:] {
		if !tags[version] {
			t.Errorf("CHANGELOG.md has a note for %s and this clone has no tag of that "+
				"name.\nIt reads as released and cannot be checked out. Three different "+
				"things look like this here: the tag was never cut (VERSIONING.md has "+
				"the command), it was cut in another clone and this one has not fetched "+
				"it yet (git fetch --tags), or the note is describing something that was "+
				"never a version", version)
		}
	}

	inChangelog := map[string]bool{}
	for _, v := range noted {
		inChangelog[v] = true
	}
	for tag := range tags {
		if !inChangelog[tag] {
			t.Errorf("%s is tagged and CHANGELOG.md says nothing about it.\n"+
				"Somebody installing it has no way to find out whether they have to "+
				"do anything, which is the question a release note is read for", tag)
		}
	}
}

// TestTheUpgradeSectionCoversEveryInstallPath.
//
// # The defect, twice
//
// KURULUM.md had no "moving to a new version" section at all: seventeen
// sections covering installing from nothing and upgrading the schema,
// and nothing about the step between them. Section 13.5 was written to
// close that.
//
// Its first draft then covered one of the two install paths. The guide
// offers a container path and a manual one - section 1.5 makes the
// reader choose - and the new section explained how to upgrade a manual
// install only. The identical gap, one path over, in the fix for the
// gap.
//
// # What is derived
//
// The install paths are not a list kept here. Each one is a file that
// has to exist for that path to work, and the upgrade section has to
// name the command that path is driven by. A third path arrives with a
// third entry and fails this test until somebody writes its three
// lines.
func TestTheUpgradeSectionCoversEveryInstallPath(t *testing.T) {
	paths := []struct {
		name     string
		artifact string // the file that makes this path real
		command  string // what the upgrade section must tell them to run
	}{
		{"manual", "release/install.sh", "./release/install.sh"},
		{"container", "docker/compose.yml", "docker compose up -d"},
	}

	body := readDoc(t, "KURULUM.md")
	const heading = "## 13.5"
	start := strings.Index(body, heading)
	if start < 0 {
		t.Fatalf("KURULUM.md has no %s section. A guide that explains installing "+
			"and explains upgrading the schema, with nothing in between, leaves "+
			"its reader at the one step they will take more than once", heading)
	}
	section := body[start:]
	if next := strings.Index(section[len(heading):], "\n## "); next >= 0 {
		section = section[:next+len(heading)]
	}

	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(repoRoot(t), p.artifact)); err != nil {
			t.Errorf("the %s install path names %s and it is not there: %v",
				p.name, p.artifact, err)
			continue
		}
		if !strings.Contains(section, p.command) {
			t.Errorf("%s does not tell a %s installation how to upgrade.\n"+
				"It is a real install path - %s is in this repository - and its "+
				"reader gets the other path's instructions, which do not apply to "+
				"them.\nExpected the section to name: %s",
				heading, p.name, p.artifact, p.command)
		}
	}
}

// TestEveryTagPointsAtSomethingOnTheDefaultBranch.
//
// # What happened
//
// v0.22.0 was cut and pushed, and it landed on a commit 203 behind, on a
// line of history that is not on main at all. The release note was
// right, the version number was right, and the ref pointed at work from
// weeks earlier.
//
// Nothing said so. TestEveryReleaseNoteHasATagAndEveryTagHasANote
// compares *names*: the changelog says v0.22.0 and a tag called v0.22.0
// exists, so both sides agreed and neither was looking at where it
// pointed. `git checkout v0.22.0` would have produced a tree without the
// feature its own note describes, and the first person to notice would
// have been somebody installing it.
//
// # Why "ancestor of main" is the right question
//
// A tag in this repository is cut on main - the branch is fast-forwarded
// into it before the tag goes on. So a tag that is not reachable from
// main is not a release of this product's history, whatever it is named.
//
// It is asked of origin/main rather than the checked-out branch, because
// this runs on a feature branch too and a tag cut on main is not
// reachable from a branch that has not been merged yet.
func TestEveryTagPointsAtSomethingOnTheDefaultBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; this test asks git where the tags point")
	}
	root := repoRoot(t)

	out, err := exec.Command("git", "-C", root, "tag", "--list", "v*").Output()
	if err != nil {
		t.Skipf("git tag failed: %v", err)
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tags = append(tags, line)
		}
	}
	if len(tags) == 0 {
		// The same reason the note-and-tag check gives: a checkout
		// without tags cannot answer this, and saying so is not the same
		// as passing.
		t.Skip("this checkout has no tags, so any answer here would be about the " +
			"checkout rather than the repository. Fetch them (git fetch --tags) to " +
			"run this check")
	}

	// Whichever of these exists. A clone has origin/main; a checkout that
	// somebody made the branch in directly has main.
	var base string
	for _, ref := range []string{"origin/main", "main"} {
		if err := exec.Command("git", "-C", root, "rev-parse", "--verify",
			"--quiet", ref+"^{commit}").Run(); err == nil {
			base = ref
			break
		}
	}
	if base == "" {
		t.Skip("neither origin/main nor main is in this checkout, so there is " +
			"nothing to measure the tags against")
	}

	for _, tag := range tags {
		err := exec.Command("git", "-C", root, "merge-base", "--is-ancestor",
			tag+"^{commit}", base).Run()
		if err == nil {
			continue
		}
		at, _ := exec.Command("git", "-C", root, "log", "--format=%h %s",
			"-1", tag+"^{commit}").Output()
		t.Errorf("%s points at a commit that is not on %s:\n    %s\n"+
			"A tag is cut on the default branch, after the branch is merged into "+
			"it. One that is not reachable from there checks out a tree that is "+
			"not this product's history at that version - and the release note "+
			"beside it still reads as correct, because a note and a tag agree on "+
			"the name without either one looking at where it points.\n"+
			"Fix: git tag -d %s && git push origin :refs/tags/%s, then cut it "+
			"again on the right commit.",
			tag, base, strings.TrimSpace(string(at)), tag, tag)
	}
}
