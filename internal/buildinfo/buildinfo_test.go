package buildinfo

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

// TestVersion_PrefersTheStamp. The linker stamp is what a person means by
// a version - a tag name, something to say out loud - so it wins over the
// revision even though both are available in a normal build.
func TestVersion_PrefersTheStamp(t *testing.T) {
	if got := Version("v0.4.1"); got != "v0.4.1" {
		t.Errorf("Version(%q) = %q, want the stamp", "v0.4.1", got)
	}
	// Whitespace is not a version. A Makefile that expands an unset
	// variable produces " ", and reporting that as a version would print
	// a blank where an operator expects a name.
	if got := Version("   "); got == "   " {
		t.Error("a whitespace-only stamp was reported as a version")
	}
}

// TestVersion_FallsBackToTheRevision.
//
// This test runs inside a normal `go test`, which builds from the working
// tree, so the binary under test carries Go's embedded VCS data. That
// makes it the real measurement rather than a mocked one: with no stamp,
// Version must produce the commit rather than "unknown".
//
// It tolerates the case where the data is genuinely absent - a build with
// -buildvcs=false, or a source tree that is not a repository - because
// asserting on it there would fail for a reason that has nothing to do
// with this code.
func TestVersion_FallsBackToTheRevision(t *testing.T) {
	rev, modified, ok := vcs()
	if !ok {
		t.Skip("this build carries no VCS data (-buildvcs=false, or not a repository)")
	}

	got := Version("")
	if got == Unknown {
		t.Fatalf("Version(\"\") = %q while the binary knows it came from %s", got, rev)
	}
	if !strings.HasPrefix(got, rev) {
		t.Errorf("Version(\"\") = %q, want it to start with the revision %q", got, rev)
	}
	// A binary built from a dirty tree corresponds to no commit, so
	// naming the commit alone would name source that is not what is
	// running.
	if modified && !strings.HasSuffix(got, "-dirty") {
		t.Errorf("Version(\"\") = %q for a modified tree, want a -dirty suffix", got)
	}
	if !modified && strings.HasSuffix(got, "-dirty") {
		t.Errorf("Version(\"\") = %q for a clean tree", got)
	}
}

// TestVersion_ShortensTheRevision. Forty hex characters is not something
// an operator reads to somebody on a phone.
func TestVersion_ShortensTheRevision(t *testing.T) {
	rev, _, ok := vcs()
	if !ok {
		t.Skip("no VCS data in this build")
	}
	if len(rev) > 12 {
		t.Errorf("revision %q is %d characters; it should have been shortened", rev, len(rev))
	}
}

// TestLine_CarriesWhatASupportConversationNeeds.
//
// Name, version, toolchain, platform. "Which build" and "built how" are
// the same question when the report is about a crash.
func TestLine_CarriesWhatASupportConversationNeeds(t *testing.T) {
	got := Line("collector", "v1.2.3")

	for _, want := range []string{"collector", "v1.2.3", runtime.Version(), runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("Line() = %q, missing %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("Line() = %q, want one line with no newline of its own", got)
	}
}

// TestPrint_EndsWithExactlyOneNewline, because it is read by a person at
// a shell and sometimes by a script.
func TestPrint_EndsWithExactlyOneNewline(t *testing.T) {
	var buf bytes.Buffer
	Print(&buf, "panel", "v9")

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Print wrote %q with no trailing newline", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("Print wrote %d newlines, want 1: %q", strings.Count(out, "\n"), out)
	}
}

// TestVersion_NeverReturnsEmpty. Whatever the inputs, something readable
// comes out: an operator reading "version:" with nothing after it cannot
// tell an unstamped build from a broken command.
func TestVersion_NeverReturnsEmpty(t *testing.T) {
	for _, stamped := range []string{"", " ", "\t\n", "v1"} {
		if got := Version(stamped); strings.TrimSpace(got) == "" {
			t.Errorf("Version(%q) returned nothing readable", stamped)
		}
	}
}
