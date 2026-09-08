// The fuzz wrapper decides what a red fuzz run means.
//
// It is the one script in this repository whose job is to *not* fail on
// something, which makes it the one that has to be tested from both
// sides. The half that matters is not "a deadline passes" - it is that
// a real finding still fails, in every shape a finding arrives in.
//
// Driven with a stub `go` rather than with the real fuzzer: a test that
// waited five minutes for a deadline would run once and then be
// skipped, and the crasher case cannot be produced on demand at all.
// What is under test is the decision, and the decision reads a
// transcript and a directory.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzScript is the wrapper's path, from this package's directory.
const fuzzScript = "fuzz.sh"

// readScript reads the wrapper, and exists so that `go test` knows this
// package's result depends on it.
//
// Not defensive noise. Every test below runs the script through `sh`,
// so the test binary never opens it - and cmd/go builds its cache key
// from the files the *test binary* reads. Without this the package's
// last result is reused after the script changes, which is not a
// theory: five mutations of fuzz.sh in a row came back green from the
// cache while the file on disk was broken.
//
// *Ölçtüğü dosyaya bağımlı olmayan bir test, ölçmeden geçebilir.*
func readScript(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(fuzzScript)
	if err != nil {
		t.Fatalf("reading the script under test: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("the script under test is empty")
	}
	return string(body)
}

// stubGo writes a fake `go` that prints transcript and exits with code.
//
// A shell script rather than a Go binary: it has to be executable, take
// the same arguments, and do one thing, and building a binary per case
// would make five cases into five compilations.
func stubGo(t *testing.T, transcript string, code int, alsoWrite string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "go")

	body := "#!/bin/sh\ncat <<'TRANSCRIPT'\n" + transcript + "\nTRANSCRIPT\n"
	if alsoWrite != "" {
		// The crasher: `go test` writes the failing input into the
		// package's corpus directory before it exits.
		body += "mkdir -p " + filepath.Dir(alsoWrite) + "\n"
		body += "printf 'go test fuzz v1\\nstring(\"x\")\\n' > " + alsoWrite + "\n"
	}
	body += "exit " + itoa(code) + "\n"

	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// runFuzzScript runs the wrapper with a stubbed `go` and returns its
// exit code and combined output.
func runFuzzScript(t *testing.T, goPath, target, pkg, fuzztime string) (int, string) {
	t.Helper()

	readScript(t) // so the cache knows this result depends on the script

	cmd := exec.Command("sh", fuzzScript, target, pkg, fuzztime)
	cmd.Env = append(os.Environ(), "GO="+goPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exit *exec.ExitError
	if ok := asExitError(err, &exit); !ok {
		t.Fatalf("running %s: %v\n%s", fuzzScript, err, out)
	}
	return exit.ExitCode(), string(out)
}

func asExitError(err error, out **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*out = e
	}
	return ok
}

// deadlineTranscript is what nightly #18 actually printed, trimmed.
const deadlineTranscript = `fuzz: elapsed: 4m57s, execs: 1236310 (323/sec), new interesting: 9 (total: 156)
fuzz: elapsed: 5m0s, execs: 1250032 (4577/sec), new interesting: 9 (total: 156)
--- FAIL: FuzzAFileThisBuildWroteIsAFileThisBuildCanRead (301.01s)
    context deadline exceeded
FAIL
exit status 1
FAIL	github.com/cruciblelab/crucible-analytic/internal/botdata	301.019s`

// TestTheWrapperPassesWhenTheClockRanOut.
func TestTheWrapperPassesWhenTheClockRanOut(t *testing.T) {
	stub := stubGo(t, deadlineTranscript, 1, "")

	code, out := runFuzzScript(t, stub,
		"FuzzAFileThisBuildWroteIsAFileThisBuildCanRead", "./internal/botdata/", "5m")

	if code != 0 {
		t.Errorf("the wrapper exited %d for a run that found nothing:\n%s", code, out)
	}
	// And it says so. A step that turns red into green silently is a
	// step nobody can audit afterwards.
	if !strings.Contains(out, "found nothing") {
		t.Errorf("the wrapper passed without saying why:\n%s", out)
	}
	// The transcript is not swallowed either.
	if !strings.Contains(out, "--- FAIL") {
		t.Errorf("the wrapper hid the transcript it decided about:\n%s", out)
	}
}

// TestTheWrapperFailsWhenTheFuzzerWroteACrasher.
//
// The direction the whole script has to be right about. Both signals a
// crasher leaves are tested, separately, because either one alone would
// be enough to make this fail and a wrapper that relied on the other
// would be one message away from silence.
func TestTheWrapperFailsWhenTheFuzzerWroteACrasher(t *testing.T) {
	// A corpus directory this test owns, inside a package path that does
	// not exist - the stub never compiles anything, so the path only has
	// to be somewhere the script can look.
	pkgDir := t.TempDir()
	corpus := filepath.Join(pkgDir, "testdata", "fuzz", "FuzzX", "abc123")

	t.Run("a file appears in the corpus", func(t *testing.T) {
		// A transcript that says nothing but the deadline: the wrapper
		// must still fail, because the directory says otherwise. This is
		// the belt-and-braces case, and it is the one that catches a
		// future Go release changing its wording.
		stub := stubGo(t, deadlineTranscript, 1, corpus)

		code, out := runFuzzScript(t, stub, "FuzzX", pkgDir, "5m")
		if code == 0 {
			t.Errorf("the wrapper passed a run that wrote %s:\n%s", corpus, out)
		}
		if !strings.Contains(out, "regression case") {
			t.Errorf("the wrapper did not say a finding was recorded:\n%s", out)
		}
		if err := os.RemoveAll(filepath.Join(pkgDir, "testdata")); err != nil {
			t.Fatal(err)
		}
	})

	// The dangerous shape: a crasher found in the last second of the
	// budget. Everything the deadline rule looks at agrees with a
	// timeout - one failure, at the end of the budget, and the words
	// are there - so the only thing that tells them apart is that the
	// fuzzer recorded an input.
	t.Run("a failing input recorded at the end of the budget", func(t *testing.T) {
		const late = `fuzz: elapsed: 5m0s, execs: 1250032 (4577/sec), new interesting: 9 (total: 156)
--- FAIL: FuzzX (301.01s)
    fuzz_test.go:41: Load could not read the file Save just wrote
    context deadline exceeded
    Failing input written to testdata/fuzz/FuzzX/9f8e7d
    To re-run:
    go test -run=FuzzX/9f8e7d
FAIL`
		stub := stubGo(t, late, 1, "")

		code, out := runFuzzScript(t, stub, "FuzzX", pkgDir, "5m")
		if code == 0 {
			t.Errorf("the wrapper passed a crasher found at the end of the budget:\n%s", out)
		}
	})

	t.Run("the transcript names a failing input", func(t *testing.T) {
		const crash = `--- FAIL: FuzzX (12.34s)
    fuzz_test.go:41: Load could not read the file Save just wrote
    Failing input written to testdata/fuzz/FuzzX/abc123
    To re-run:
    go test -run=FuzzX/abc123
FAIL`
		stub := stubGo(t, crash, 1, "")

		code, out := runFuzzScript(t, stub, "FuzzX", pkgDir, "5m")
		if code == 0 {
			t.Errorf("the wrapper passed a run that recorded a failing input:\n%s", out)
		}
	})
}

// TestTheWrapperFailsWhenTheFailureIsNotTheDeadline.
//
// Three shapes, and the third is the subtle one: a seed corpus entry
// that fails does so in the first second, so "it says context deadline
// exceeded" cannot be the whole rule - the failure has to have arrived
// when the budget ran out.
func TestTheWrapperFailsWhenTheFailureIsNotTheDeadline(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
	}{
		{"an assertion", `--- FAIL: FuzzX (3.10s)
    fuzz_test.go:99: wrote "a"="b" and read back map[]
FAIL`},
		{"the package does not build", `# github.com/cruciblelab/crucible-analytic/internal/botdata
./botdata.go:12:2: undefined: nope
FAIL	github.com/cruciblelab/crucible-analytic/internal/botdata [build failed]`},
		// The one that would walk through a rule written only on the
		// message: a target whose own code reports a context deadline,
		// failing immediately rather than at the end of the budget.
		{"a deadline that arrived at once", `--- FAIL: FuzzX (0.00s)
    fuzz_test.go:12: talking to the database: context deadline exceeded
FAIL`},
		// A real failure standing beside the deadline. Without the
		// "exactly one failure" rule, the deadline line alone would be
		// enough to clear a transcript that also reports a defect.
		//
		// Both orderings, because the rule must not depend on which line
		// go test prints first: with the deadline first, the elapsed
		// time read from the transcript is the deadline's own, and the
		// count is the only thing left standing between a defect and a
		// green step.
		{"a real failure after the deadline", `--- FAIL: FuzzX (301.01s)
    context deadline exceeded
--- FAIL: FuzzOther (0.02s)
    other_test.go:7: the seed corpus does not round trip
FAIL`},
		{"a real failure before the deadline", `--- FAIL: FuzzOther (0.02s)
    other_test.go:7: the seed corpus does not round trip
--- FAIL: FuzzX (301.01s)
    context deadline exceeded
FAIL`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubGo(t, tc.transcript, 1, "")
			code, out := runFuzzScript(t, stub, "FuzzX", "./internal/botdata/", "5m")
			if code == 0 {
				t.Errorf("the wrapper passed a real failure:\n%s", out)
			}
		})
	}
}

// TestTheWrapperPassesAGreenRunThrough.
func TestTheWrapperPassesAGreenRunThrough(t *testing.T) {
	const green = `fuzz: elapsed: 5m0s, execs: 4250032 (14577/sec), new interesting: 3 (total: 88)
PASS
ok  	github.com/cruciblelab/crucible-analytic/internal/botdata	300.019s`

	stub := stubGo(t, green, 0, "")
	code, out := runFuzzScript(t, stub, "FuzzX", "./internal/botdata/", "5m")
	if code != 0 {
		t.Errorf("the wrapper turned a green run red (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "ok  ") {
		t.Errorf("the wrapper did not print what go test said:\n%s", out)
	}
}

// TestTheWrapperRefusesABudgetItCannotRead.
//
// The deadline rule compares the failure's elapsed time against the
// budget, so a budget the script cannot parse would silently make that
// comparison meaningless. It refuses instead.
func TestTheWrapperRefusesABudgetItCannotRead(t *testing.T) {
	stub := stubGo(t, deadlineTranscript, 1, "")
	for _, bad := range []string{"5 minutes", "abc", "0s"} {
		code, out := runFuzzScript(t, stub, "FuzzX", "./internal/botdata/", bad)
		if code == 0 {
			t.Errorf("the wrapper accepted the budget %q:\n%s", bad, out)
		}
	}
}
