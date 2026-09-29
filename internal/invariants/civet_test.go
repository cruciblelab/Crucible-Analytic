package invariants

import (
	"go/build/constraint"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every file behind a build constraint is compiled by something in
// ci.yml.
//
// # What was wrong
//
// CONTRIBUTING.md's "What CI runs" block said CI vetted every tag:
//
//	for tag in loadtest network release e2e docker; do go vet -tags "$tag" ./...; done
//
// ci.yml vetted three of them, two over a single directory. The loop
// was the local gate's (release/gate.sh), and a file under `network`,
// `e2e` or `docker` compiled on a pull request only if its author had
// run the gate. Found on 2026-09-29, adding the systemd suite and asking
// where it would be vetted.
//
// # Why per file, with Go's own evaluator
//
// Because a tag is not what gets compiled - a file is, when its whole
// constraint holds. A file under "e2e && systemd" is compiled by neither
// `-tags e2e` nor `-tags systemd`, and a check that asked about tags one
// at a time would call it covered. constraint.Expr.Eval answers the
// question the compiler asks.
//
// # Two kinds of compilation, and only one of them sees tests
//
// The vet step runs on the linux/amd64 runner with a tag set and
// compiles test files too. The cross-compile matrix runs `go build
// ./...` for four GOOS/GOARCH pairs and no tags, and never compiles a
// _test.go file. So a non-test file under `!linux` is covered by the
// darwin and windows builds - the first run of this test reported
// internal/diskspace/diskspace_other.go as uncompiled, because it asked
// only the vet step - while a test file under `!linux` would not be.

// civetLine matches one `go vet` invocation in a workflow and captures
// its tags (possibly none) and the package pattern.
var civetLine = regexp.MustCompile(`^go vet(?: -tags[= ]"?([a-z0-9,]+)"?)? (\S+)$`)

// compileCall is one thing in ci.yml that compiles Go files.
type compileCall struct {
	name        string // for the failure message
	tags        map[string]bool
	goos        string
	goarch      string
	cgo         bool
	pattern     string // "./..." or "./dir/" or "./dir/..."
	compilesTst bool   // vet compiles _test.go files; go build does not
}

func (c compileCall) covers(dir string) bool {
	p := strings.TrimPrefix(c.pattern, "./")
	switch {
	case p == "...":
		return true
	case strings.HasSuffix(p, "/..."):
		base := strings.TrimSuffix(p, "/...")
		return dir == base || strings.HasPrefix(dir, base+"/")
	default:
		return dir == strings.TrimSuffix(p, "/")
	}
}

// unixGOOS are the GOOS values the `unix` constraint term holds for,
// as far as this matrix can name them.
var unixGOOS = map[string]bool{"linux": true, "darwin": true, "freebsd": true,
	"netbsd": true, "openbsd": true, "dragonfly": true, "solaris": true, "aix": true,
	"android": true, "illumos": true, "ios": true}

func (c compileCall) satisfies(expr constraint.Expr) bool {
	return expr.Eval(func(tag string) bool {
		switch {
		case c.tags[tag]:
			return true
		case tag == c.goos || tag == c.goarch:
			return true
		case tag == "unix":
			return unixGOOS[c.goos]
		case tag == "cgo":
			return c.cgo
		case strings.HasPrefix(tag, "go1."):
			return true
		}
		return false
	})
}

// compiledBy reports whether any call compiles a file in dir under
// expr. A test file is compiled only by a call that compiles tests.
func compiledBy(calls []compileCall, dir string, isTest bool, expr constraint.Expr) bool {
	for _, c := range calls {
		if isTest && !c.compilesTst {
			continue
		}
		if c.covers(dir) && c.satisfies(expr) {
			return true
		}
	}
	return false
}

// ciCompileCalls reads both kinds out of ci.yml.
func ciCompileCalls(t *testing.T, root string) []compileCall {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var calls []compileCall
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := civetLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// The vet step runs on ubuntu-latest, where cgo is available.
		c := compileCall{name: line, tags: map[string]bool{}, goos: "linux", goarch: "amd64",
			cgo: true, pattern: m[2], compilesTst: true}
		if m[1] != "" {
			for _, tag := range strings.Split(m[1], ",") {
				c.tags[tag] = true
			}
		}
		calls = append(calls, c)
	}

	// The cross-compile matrix, read as YAML because its targets are
	// data rather than lines.
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Target []struct {
						GOOS   string `yaml:"goos"`
						GOARCH string `yaml:"goarch"`
					} `yaml:"target"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatalf("parsing ci.yml: %v", err)
	}
	for name, job := range wf.Jobs {
		builds := false
		for _, s := range job.Steps {
			if strings.Contains(s.Run, "go build") && strings.Contains(s.Run, "./...") {
				builds = true
			}
		}
		if !builds {
			continue
		}
		for _, target := range job.Strategy.Matrix.Target {
			// Cross-compiling turns cgo off unless it is asked for, and
			// nothing here asks; the native pair keeps it.
			native := target.GOOS == "linux" && target.GOARCH == "amd64"
			calls = append(calls, compileCall{
				name: name + " " + target.GOOS + "/" + target.GOARCH, tags: map[string]bool{},
				goos: target.GOOS, goarch: target.GOARCH, cgo: native, pattern: "./...",
			})
		}
	}
	return calls
}

func TestEveryConstrainedFileIsCompiledByCI(t *testing.T) {
	root := repoRoot(t)
	calls := ciCompileCalls(t, root)
	vets, builds := 0, 0
	for _, c := range calls {
		if c.compilesTst {
			vets++
		} else {
			builds++
		}
	}
	if vets == 0 || builds == 0 {
		t.Fatalf("found %d `go vet` lines and %d cross-compile targets in ci.yml; the "+
			"workflow changed shape and this check now reads less than it should", vets, builds)
	}

	constrained := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			// The directories the go tool itself skips (dot, underscore,
			// testdata), plus the two this repository keeps out of ./...
			// by ignoring them: build output and scratch work.
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor" || name == "dist" || name == "scratchpad") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var expr constraint.Expr
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "package ") {
				break
			}
			if constraint.IsGoBuild(trimmed) {
				expr, err = constraint.Parse(trimmed)
				if err != nil {
					t.Errorf("%s: %v", path, err)
					return nil
				}
			}
		}
		if expr == nil {
			return nil
		}
		constrained++
		rel := mustRel(root, path)
		dir := filepath.ToSlash(filepath.Dir(rel))
		if compiledBy(calls, dir, strings.HasSuffix(path, "_test.go"), expr) {
			return nil
		}
		t.Errorf("%s is under //go:build %s and nothing in ci.yml compiles it.\n"+
			"A pull request that breaks it goes green; add its tag set to the vet step "+
			"(over ./..., so the next file under that tag is covered too)", rel, expr)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if constrained == 0 {
		t.Fatal("no file with a build constraint was found; either there are none left " +
			"or the scan stopped recognising them, and a scan that matches nothing reports nothing")
	}
}

// TestTheCompileScopeIsItselfMeasured holds covers and satisfies to what
// they claim, because a check that answered "covered" for everything
// would pass above without looking.
func TestTheCompileScopeIsItselfMeasured(t *testing.T) {
	all := compileCall{tags: map[string]bool{"e2e": true}, goos: "linux", goarch: "amd64",
		cgo: true, pattern: "./...", compilesTst: true}
	one := compileCall{tags: map[string]bool{}, goos: "linux", goarch: "amd64", pattern: "./internal/loadtest/"}
	tree := compileCall{tags: map[string]bool{}, goos: "linux", goarch: "amd64", pattern: "./internal/..."}
	win := compileCall{tags: map[string]bool{}, goos: "windows", goarch: "amd64", pattern: "./..."}

	for _, c := range []struct {
		call compileCall
		dir  string
		want bool
	}{
		{all, "e2e", true},
		{one, "internal/loadtest", true},
		{one, "internal/asnlookup", false},
		{tree, "internal/asnlookup", true},
		{tree, "e2e", false},
	} {
		if got := c.call.covers(c.dir); got != c.want {
			t.Errorf("%q covers %q = %v, want %v", c.call.pattern, c.dir, got, c.want)
		}
	}

	parse := func(s string) constraint.Expr {
		e, err := constraint.Parse("//go:build " + s)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, c := range []struct {
		call compileCall
		expr string
		want bool
	}{
		{all, "e2e", true},
		{all, "e2e && systemd", false}, // the case per-tag checking gets wrong
		{all, "e2e || systemd", true},
		{all, "windows", false},
		{all, "integration && linux", false}, // integration is not in this call's set
		{all, "!integration", true},
		{all, "!linux", false},
		{win, "!linux", true},
		{win, "unix", false},
		{win, "cgo", false},
	} {
		if got := c.call.satisfies(parse(c.expr)); got != c.want {
			t.Errorf("%s/%s %v satisfies %q = %v, want %v",
				c.call.goos, c.call.goarch, c.call.tags, c.expr, got, c.want)
		}
	}

	// The two kinds together: the windows build compiles a non-test file
	// under !linux, and nothing compiles a test file under it - go build
	// never reads a _test.go.
	both := []compileCall{all, win}
	if !compiledBy(both, "internal/diskspace", false, parse("!linux")) {
		t.Error("a non-test file under !linux is compiled by the windows build and was reported as not")
	}
	if compiledBy(both, "internal/diskspace", true, parse("!linux")) {
		t.Error("a test file under !linux was reported as compiled by go build, which never reads tests")
	}
}
