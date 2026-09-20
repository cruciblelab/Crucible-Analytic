// The gate refuses --all without the connection its fixtures clean up
// with, rather than running and blaming the product.
//
// # The run this is written after
//
// `release/gate.sh --all` went red on 2026-09-20 with thirty-odd failing
// tests across five packages: "that email address is already
// registered", "16 rows reached the table; only the WARN should have".
// None of it was the product. CA_SUPERUSER_DSN was unset, so the
// fixtures that delete their rows as the schema's owner skipped that
// step - quietly, because `go test` prints a test's log lines only when
// that test fails, and the test that leaves the row behind is the one
// that passed. The next test in the same run then landed on it.
//
// The same tree at the same commit went green with the variable set.
// That is the whole diagnosis, and it took an hour of reading a
// transcript to reach, because every line of that transcript pointed at
// the product.
//
// So the question is asked once, up front. What is checked here is both
// directions - a refusal without it, and no refusal with it - because a
// guard measured only where it fires is a guard whose condition is
// unmeasured: `[ -z ... ]` inverted to `[ -n ... ]` passes a one-sided
// test.
package release

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runGateAll runs `gate.sh --all` with CA_SUPERUSER_DSN set to dsn, or
// removed from the environment when dsn is empty.
//
// CA_GATE_ONLY is pinned to one cheap step so the half of this test that
// expects the gate to *proceed* costs a second rather than the ten
// minutes the integration step costs. That filter cannot hide the
// behaviour under test: the refusal is not a step, so no filter reaches
// it.
func runGateAll(t *testing.T, dsn string) (string, int) {
	t.Helper()

	// Read the script, so `go test` knows this package's result depends
	// on it. Without this the cache answers from a previous build and a
	// mutation of gate.sh comes back green - which is exactly what
	// release/fuzz_test.go was written after.
	if _, err := os.ReadFile(gateScript); err != nil {
		t.Fatalf("reading the script under test: %v", err)
	}

	env := withoutGateLog(os.Environ())
	kept := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "CA_SUPERUSER_DSN=") {
			continue
		}
		kept = append(kept, kv)
	}
	kept = append(kept, "CA_GATE_ONLY=gofmt")
	if dsn != "" {
		kept = append(kept, "CA_SUPERUSER_DSN="+dsn)
	}

	cmd := exec.Command("bash", gateScript, "--all")
	cmd.Env = kept
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running %s --all: %v\n%s", gateScript, err, out.String())
		}
		code = exit.ExitCode()
	}
	return out.String(), code
}

// Without the variable, --all stops - and says which variable, before it
// has run anything.
func TestTheGateRefusesAllWithoutASuperuserDSN(t *testing.T) {
	out, code := runGateAll(t, "")

	if code == 0 {
		t.Fatalf("the gate ran --all without CA_SUPERUSER_DSN and exited 0:\n%s", out)
	}
	// The *first* line names it, not merely the output somewhere.
	//
	// Measured: asking whether the whole output contains the name is an
	// assertion the example line at the bottom satisfies on its own, so
	// a headline reading "gate: RED - a variable is missing" survived
	// it. A reader who scans the top line of a red gate and learns
	// nothing is the reader this refusal exists for.
	first := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	if !strings.Contains(first, "CA_SUPERUSER_DSN") {
		t.Errorf("the refusal's first line does not name the variable:\n%s", out)
	}
	// Refused where the answer is one line, not after the nine steps that
	// come before the integration half. Measured by what the gate printed:
	// a step announces itself with "== <name>".
	if strings.Contains(out, "== gofmt") {
		t.Errorf("the gate ran a step before refusing; a precondition checked after "+
			"ten minutes of work costs ten minutes to learn:\n%s", out)
	}
	// The wording that makes it actionable rather than merely true.
	if !strings.Contains(out, "leave rows behind") {
		t.Errorf("the refusal says what is missing but not what goes wrong without "+
			"it, which is the half that stops the next reader from looking for a "+
			"product defect:\n%s", out)
	}
}

// With the variable, --all proceeds. This is the other side of the
// threshold: without it, a guard that refused unconditionally - or one
// whose test was inverted - would pass the test above.
//
// The DSN is never connected to here, so this does not need a database.
// The gate does not dial it; it asks whether the caller supplied one.
func TestTheGateRunsAllWhenTheSuperuserDSNIsSet(t *testing.T) {
	out, code := runGateAll(t, "postgres://nobody@127.0.0.1:1/never?sslmode=disable")

	if code != 0 {
		t.Fatalf("the gate exited %d for a filtered --all with CA_SUPERUSER_DSN "+
			"set:\n%s", code, out)
	}
	if strings.Contains(out, "gate: RED - --all needs") {
		t.Errorf("the gate refused --all although CA_SUPERUSER_DSN was set:\n%s", out)
	}
	// It got past the guard and into the steps - otherwise "exited 0"
	// would also describe a gate that refused with the wrong status.
	if !strings.Contains(out, "== gofmt") {
		t.Errorf("the gate exited 0 without running the step it was filtered to; "+
			"that is a pass this test cannot tell from a refusal:\n%s", out)
	}
}
