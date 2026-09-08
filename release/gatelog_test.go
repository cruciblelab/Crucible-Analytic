// The gate keeps its own transcript.
//
// # The run this is written after
//
// `release/gate.sh --all` went red on one integration test, and the
// message was gone. The run had been piped through grep to keep the
// summary lines - and the summary lines are the ones that carry no
// numbers. Three re-runs passed, so the single run that had something to
// say was the one nobody could read.
//
// The script now writes the whole run to a file and prints the path on
// stderr, where a filter on stdout cannot take it. What is checked here
// is that the file exists, holds what the terminal showed, and is named
// where the script said it was - and that a filtered run cannot be
// mistaken for a full one.
//
// Fast, because of the filter it also tests: one step, not nine.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gateScript is the gate, from this package's directory.
const gateScript = "gate.sh"

var transcriptLine = regexp.MustCompile(`full transcript: (\S+)`)

// runGate runs the gate with a step filter and returns stdout, stderr
// and the exit code.
func runGate(t *testing.T, only string) (string, string, int) {
	t.Helper()

	// Read the script, so `go test` knows this package's result depends
	// on it: every test here runs it through a shell, and the cache is
	// built from what the *test binary* opens. See release/fuzz_test.go,
	// where five mutations came back green from a stale cache.
	if _, err := os.ReadFile(gateScript); err != nil {
		t.Fatalf("reading the script under test: %v", err)
	}

	cmd := exec.Command("bash", gateScript)
	// CA_GATE_ONLY is the arrangement; CA_GATE_LOG must not be. It is
	// the handshake between the gate's two passes, and a gate that
	// inherits it skips the wrapping this test is here to measure -
	// which is exactly what happened when these tests were first run
	// from inside a gate step.
	cmd.Env = append(withoutGateLog(os.Environ()), "CA_GATE_ONLY="+only)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running %s: %v\n%s", gateScript, err, errOut.String())
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// TestTheGateLeavesATranscriptOnDisk.
func TestTheGateLeavesATranscriptOnDisk(t *testing.T) {
	stdout, stderr, code := runGate(t, "gofmt")
	if code != 0 {
		t.Fatalf("the gate exited %d for the gofmt step alone:\n%s\n%s", code, stdout, stderr)
	}

	// The path is on stderr rather than stdout, which is the half that
	// makes it survive `gate.sh | grep`.
	m := transcriptLine.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("the gate did not name its transcript on stderr:\n%s", stderr)
	}
	path := m[1]
	if strings.Contains(stdout, "full transcript:") {
		t.Error("the transcript path is on stdout, where a filter takes it with " +
			"everything else it is filtering out")
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the gate named a transcript it did not write: %v", err)
	}
	// Everything the terminal saw. Compared as containment rather than
	// as equality: the file also holds the step's own output, which is
	// the point of having it.
	for _, want := range []string{"== gofmt", "gate: green"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the transcript at %s does not contain %q:\n%s", path, want, body)
		}
	}
	if !strings.Contains(stdout, "== gofmt") {
		t.Errorf("the gate wrote a transcript and showed nothing:\n%s", stdout)
	}
}

// TestAFilteredGateDoesNotCallItselfGreen.
//
// Two ways a filtered run could lie, and both are refused. Saying the
// plain words "gate: green" after running one check of nine is the first;
// the second is quieter and worse - a filter that matches nothing runs no
// check at all, and a script that reported that as success would be a
// gate anybody could pass by typing a name wrong.
func TestAFilteredGateDoesNotCallItselfGreen(t *testing.T) {
	t.Run("a filtered pass says it was filtered", func(t *testing.T) {
		stdout, _, code := runGate(t, "gofmt")
		if code != 0 {
			t.Fatalf("the gate exited %d:\n%s", code, stdout)
		}
		final := lastLine(stdout)
		if !strings.Contains(final, "not the whole gate") {
			t.Errorf("a filtered run ended with %q, which reads as a full green", final)
		}
	})

	t.Run("a filter that matches nothing is red", func(t *testing.T) {
		stdout, _, code := runGate(t, "bir-adimin-adi-degil")
		if code == 0 {
			t.Errorf("the gate was green having run no step at all:\n%s", stdout)
		}
		if !strings.Contains(stdout, "no step matched") {
			t.Errorf("the gate did not say why it was red:\n%s", stdout)
		}
	})
}

// withoutGateLog drops CA_GATE_LOG from an environment.
func withoutGateLog(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "CA_GATE_LOG=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}

// TestTheTranscriptGoesSomewhereWritable.
//
// The path is built from TMPDIR, which is not always /tmp - a CI runner
// sets it elsewhere, and a sandbox may make /tmp read-only. If the file
// cannot be written the gate has quietly lost the thing this whole
// change exists to keep.
func TestTheTranscriptGoesSomewhereWritable(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("bash", gateScript)
	cmd.Env = append(withoutGateLog(os.Environ()), "CA_GATE_ONLY=gofmt", "TMPDIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the gate failed with TMPDIR=%s: %v\n%s", dir, err, out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ca-gate-") {
			found = filepath.Join(dir, e.Name())
		}
	}
	if found == "" {
		t.Fatalf("no transcript in TMPDIR=%s; the gate wrote it somewhere else:\n%s", dir, out)
	}
}
