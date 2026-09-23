package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A test suite that never runs is worse than a missing one.
//
// Missing is visible: somebody asks "is this covered" and the answer is
// no. A suite behind a build tag that no workflow names looks covered
// from every angle - the files are there, they compile (the gate vets
// every tag), and `go test -tags loadtest ./internal/asnlookup/` passes
// on the machine of whoever wrote them. It just never runs again.
//
// This is not hypothetical. internal/asnlookup carried three
// loadtest-tagged tests, one of them the proof that local_csv_path never
// touches the network, and the nightly's load job named one directory:
// ./internal/loadtest/. They had never run in CI. Adding the network
// suite for M1 to a job whose command was a hand-written path is what
// prompted looking, which is the sixth time this season that a list was
// dangerous by being short rather than by being wrong.
//
// So both sides are read from files: the tags out of the test sources,
// the commands out of the workflows.

// gatedTags are the build tags whose suites do not run in the default
// gate, with what each one depends on.
//
// The reason each is here rather than in the ordinary suite is the
// dependency, and naming it is what stops somebody "simplifying" one
// into the gate: a gate that goes red because a third party's web server
// is down teaches people to ignore red.
var gatedTags = map[string]string{
	"network":  "a third party's web server, up when they decide it is",
	"loadtest": "real concurrency and timing, too slow and too shared-runner-dependent to gate",
	"e2e":      "the whole chain built and running, minutes per run",
	"docker":   "a built image and a container runtime",
	"release":  "a full reproducible build, tens of minutes",
}

// integration is deliberately absent: ci.yml runs it over ./... on every
// push, so it is gated, not scheduled.

// platformTerms are build constraints that select a machine rather than
// a suite.
//
// The distinction this test got wrong at first. A tag like `network`
// says "these tests need something the gate cannot promise"; a term like
// `linux` says "this code only compiles here". The second is not a gate
// and has no workflow job, and treating it as one reported
// internal/diskspace - which runs in the ordinary gate, on Linux, on
// every push - as a suite nothing executes.
//
// Left in and the file would have been forced to carry a tag it does not
// need, or the invariant would have been switched off for it. Both are
// the shape this whole file argues against: a check bent until it stops
// complaining is a check that has stopped looking.
var platformTerms = map[string]bool{
	"linux": true, "darwin": true, "windows": true, "unix": true,
	"freebsd": true, "netbsd": true, "openbsd": true, "js": true, "wasip1": true,
	"amd64": true, "arm64": true, "386": true, "arm": true, "riscv64": true,
	"cgo": true,
}

// buildTag matches "//go:build network" and "//go:build e2e || docker".
var buildTag = regexp.MustCompile(`^//go:build (.+)$`)

// goTestLine matches a workflow's test command and captures the tag list
// and everything after it, which is where the package paths are.
//
// Digits belong in the class: without them "e2e" captured as "e", the
// e2e job looked like it ran a tag nothing carries, and the e2e suite
// looked unrun. Which is to say this regexp made exactly the mistake the
// test exists to catch, and the test caught it on its first run.
var goTestLine = regexp.MustCompile(`go test [^\n]*-tags[= ]"?([a-z0-9,]+)"?([^\n]*)`)

// TestEveryGatedSuiteIsRunSomewhere.
//
// The direction that catches a suite nobody executes.
func TestEveryGatedSuiteIsRunSomewhere(t *testing.T) {
	root := repoRoot(t)

	suites := taggedSuites(t, root)
	if len(suites) == 0 {
		t.Fatal("no build-tagged test files found; this test would pass by checking " +
			"nothing, which is exactly how it would look if the scan broke")
	}
	commands := workflowTestCommands(t, root)
	if len(commands) == 0 {
		t.Fatal("no tagged `go test` commands found in .github/workflows; either the " +
			"workflows changed shape or this scan did, and both mean every suite " +
			"below would be reported as unrun")
	}

	for _, s := range sortedSuiteKeys(suites) {
		tag, pkg := s.tag, s.pkg
		if _, gated := gatedTags[tag]; !gated {
			t.Errorf("%s carries //go:build %s and that tag is not in gatedTags.\n"+
				"Either it runs in the ordinary gate - in which case the tag is doing "+
				"nothing - or it is a new kind of dependency that needs a line here "+
				"saying what it needs and why it cannot gate a merge", pkg, tag)
			continue
		}
		if !someCommandRuns(commands, tag, pkg) {
			t.Errorf("%s has //go:build %s tests and no workflow runs them.\n"+
				"It compiles (the gate vets every tag) and it passes wherever somebody "+
				"runs it by hand, so nothing anywhere reports the day it stops.\n"+
				"Add the package to the job that runs -tags %s.", pkg, tag, tag)
		}
	}
}

// TestEveryGatedTagStillHasASuite.
//
// The stale half. A tag listed above with no tests left describes a
// dependency this project no longer has, and the workflow job that runs
// it is then a green step that executes nothing - which reads, on the
// summary page, exactly like a job that passed.
func TestEveryGatedTagStillHasASuite(t *testing.T) {
	suites := taggedSuites(t, repoRoot(t))

	present := map[string]bool{}
	for s := range suites {
		present[s.tag] = true
	}
	for tag := range gatedTags {
		if !present[tag] {
			t.Errorf("gatedTags lists %q and no test file carries that tag. The workflow "+
				"job running it passes by testing nothing, which on the summary page "+
				"looks the same as passing", tag)
		}
	}
}

type suite struct{ tag, pkg string }

// taggedSuites reads every _test.go file's build constraint.
//
// Constraints are OR'd terms in this repository ("e2e || docker"), so
// each term is recorded separately: a file that runs under either tag is
// a suite under both, and the one that goes unrun is the one nobody
// thought about.
func taggedSuites(t *testing.T, root string) map[suite]bool {
	t.Helper()
	out := map[suite]bool{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "vendor" || name == "testdata" ||
				(strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The constraint is in the first few lines or it is not a
		// constraint: Go only honours it above the package clause.
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "package ") {
				break
			}
			m := buildTag.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			pkg := packagePath(root, filepath.Dir(path))
			// Split on both operators. A constraint like
			// "integration && linux" is one gate term and one platform
			// term, and reading it whole produced a tag no workflow
			// could ever name because no such tag exists.
			for _, term := range strings.FieldsFunc(m[1], func(r rune) bool {
				return r == '|' || r == '&' || r == '(' || r == ')'
			}) {
				term = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(term), "!"))
				if term == "" || term == "integration" || platformTerms[term] {
					continue
				}
				out[suite{tag: term, pkg: pkg}] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for build-tagged tests: %v", err)
	}
	return out
}

// command is one `go test -tags ...` invocation from a workflow.
type command struct {
	tags     []string
	packages []string
}

// workflowTestCommands reads every tagged test command out of the
// workflow files.
func workflowTestCommands(t *testing.T, root string) []command {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var out []command
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			// A commented-out command is not a command, and a comment
			// mentioning one is how this scan would be fooled into
			// reporting a suite as covered.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			m := goTestLine.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			c := command{tags: strings.Split(m[1], ",")}
			for _, field := range strings.Fields(m[2]) {
				if strings.HasPrefix(field, "./") {
					c.packages = append(c.packages, strings.TrimSuffix(field, "/"))
				}
			}
			out = append(out, c)
		}
	}
	return out
}

// someCommandRuns reports whether any command covers this suite.
func someCommandRuns(commands []command, tag, pkg string) bool {
	for _, c := range commands {
		if !contains(c.tags, tag) {
			continue
		}
		for _, p := range c.packages {
			if p == "./..." || p == "."+string(filepath.Separator)+pkg || p == "./"+pkg {
				return true
			}
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// packagePath turns an absolute directory into the repository-relative
// form a workflow writes, using forward slashes on every platform
// because that is what the YAML contains.
func packagePath(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}

// sortedSuiteKeys makes the failure output stable; an error list that
// reshuffles between runs is one people stop reading.
func sortedSuiteKeys(m map[suite]bool) []suite {
	out := make([]suite, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pkg != out[j].pkg {
			return out[i].pkg < out[j].pkg
		}
		return out[i].tag < out[j].tag
	})
	return out
}

// sharedRows are the tables whose contents are global to the database,
// and the lock a suite must hold before writing one.
//
// A table rather than a second copy of the test below: the schema
// version row came first, and the next one is a line. It was a line.
//
// "Single row" is how this started and it turned out to be too narrow.
// panel_users has as many rows as a deployment has accounts, and what is
// global about it is not any row but whether there are none - two of
// this product's behaviours turn on that, and a suite that empties the
// table is asserting against a condition any other package can break.
var sharedRows = []struct {
	table string
	lock  string
	// cost is what the disagreement actually did, for the failure
	// message. A test that says "these differ" invites somebody to make
	// them differ deliberately; one that says what it broke does not.
	cost string
}{
	{
		table: "schema_version",
		lock:  "SchemaVersionLock",
		cost: "It cost a CI run, and the failure named neither package: " +
			"TestTheHealthPageReportsTheSchemaVersion reported \"the page says " +
			"satırları kaybeder, which belongs to another state\" - and it did belong " +
			"to another state, one internal/panel had set from a different process " +
			"mid-assertion. Three suites write this row; two took the lock and one " +
			"did not, and the overlap stayed narrow enough to hide until " +
			"internal/panel/web grew by twenty seconds. And it cost CI 420, inside " +
			"one package: internal/panel/web took the lock in one test and wrote the " +
			"row from another, and a check that asked the package whether it named " +
			"the lock was told yes",
	},
	{
		table: "panel_users",
		lock:  "AccountsLock",
		cost: "It cost a red main on 2026-09-07. internal/panel and " +
			"internal/panel/web had taken turns over this table for weeks, with the " +
			"lock and its reasoning written out on both sides - and internal/backup's " +
			"restore suite later began creating accounts while a backup ran, which " +
			"neither copy of the constant had anything to say about. " +
			"TestStore_RealDB_BootstrapLinkDiesWhenAnAccountAppears failed with " +
			"\"expected an auto-approved request\": a link auto-approves only while " +
			"nobody owns the deployment, and somebody owned it for a few milliseconds " +
			"in another package. Two suites guarding a condition is not the same as " +
			"the condition being guarded",
	},
}

// TestEverySuiteThatWritesASharedRowTakesItsLock.
//
// # What this catches
//
// `go test ./...` runs packages in parallel, and some tables hold
// exactly one row for the whole database. A suite that writes one
// without taking its advisory lock does not fail; it makes some *other*
// package fail, later, on a different runner, with a message that names
// neither of them.
//
// That is the worst shape a test failure can have, and it has now
// happened three times in this repository for the same table. The lock
// exists and is documented; what was missing was anything that noticed a
// writer without it.
//
// # Why the write is found by pattern rather than by a list
//
// A list of suites is a list that is right on the day it is written -
// which is exactly how this got through. What is derived is the set of
// tests that write the row at all; the check is that each one also takes
// its lock.
//
// # Per test, not per package
//
// The first version asked it of the package: some test file there names
// the lock. The reasoning was that a suite takes its lock once, where it
// builds its store, and writes from wherever it likes. That was true of
// internal/panel and false of internal/panel/web, which took
// SchemaVersionLock in one test and wrote the row from another - and the
// package mentioned the lock, so the check passed. CI 420 is what it
// cost: internal/panel set the row behind, read the status, and was told
// one call later that the schema was already current, because the
// unlocked test had put back a row it had read before internal/panel's
// write. Run together on purpose, the pair left the shared row at version
// 23 in three runs out of three.
//
// The unit that holds a lock is a test - testdb.Lock releases it when
// the test that took it ends - so that is the unit asked. Every test
// that can reach a write, through the package's own helpers however
// deep, must also reach a testdb.Lock call with the row's key.
//
// Reach is the package's own call graph, over-approximated: any function
// or method its test files declare, called or referred to by name
// (t.Cleanup(restore) counts). Over-approximating is the safe direction
// for finding writes and the unsafe one for finding locks, so a lock only
// counts as the call itself - testdb.Lock(_, _, testdb.<Key>). A function
// that merely names the key, as internal/applier's check that it holds
// the lock does, is not taking it.
//
// # What it does not see
//
// Order. A test that writes and only then takes the lock reaches both,
// and so does a helper that registers its restore before it locks -
// cleanups run last in, first out, so that restore lands after the
// unlock. The helpers that write these rows lock first; nothing but
// their comments holds them to it.
//
// And writes that are not SQL in a test file. internal/applier's
// recordOn is the one production writer of schema_version; the suites
// that reach it build their own database.
func TestEverySuiteThatWritesASharedRowTakesItsLock(t *testing.T) {
	pkgs := testPackagesUnder(t, repoRootFromInvariants(t))

	for _, shared := range sharedRows {
		t.Run(shared.table, func(t *testing.T) {
			// INSERT INTO / UPDATE / DELETE FROM, allowing the newlines
			// and indentation a Go raw string puts inside SQL.
			write := regexp.MustCompile(
				`(?is)(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+` + shared.table + `\b`)

			writers := 0
			var missing []string
			for _, pkg := range pkgs {
				// A suite that builds its own database is not writing the
				// shared one, and a lock on a database it never opens
				// would protect nothing. The exemption is two conditions
				// rather than a name in a list, so it can be checked
				// rather than believed: the package must create a
				// database of its own, and it must never reach for
				// internal/testdb, which is the only way in this
				// repository to open the shared one.
				//
				// internal/retention's compression suite,
				// internal/upgradepath and internal/applier qualify, and
				// all three exist because a suite that changes a
				// database's shape must not run in the database other
				// suites are using.
				if pkg.ownDB && !pkg.sharedDB {
					continue
				}
				funcs, roots := indexTestFuncs(pkg.files, write, shared.lock)
				for _, root := range roots {
					var writes []string
					locks := false
					for _, name := range reachFrom(funcs, root) {
						if funcs[name].writes {
							writes = append(writes, strings.TrimPrefix(name, "."))
						}
						locks = locks || funcs[name].locks
					}
					if len(writes) == 0 {
						continue
					}
					writers++
					if !locks {
						missing = append(missing, pkg.rel+": "+root+" (writes in "+
							strings.Join(writes, ", ")+")")
					}
				}
			}

			if writers == 0 {
				t.Fatalf("no test writes %s, so this check is looking at nothing. "+
					"Either the table was renamed or the pattern stopped matching how "+
					"these writes are spelled", shared.table)
			}
			sort.Strings(missing)
			for _, where := range missing {
				t.Errorf("%s writes %s and never takes testdb.%s.\n\n"+
					"%s is shared by the whole database, and `go test ./...` runs "+
					"packages in parallel - so this does not fail here, it makes "+
					"another package fail somewhere else with a message that names "+
					"neither. Another test in the same package holding the lock "+
					"protects nothing: the lock is released when the test that took "+
					"it ends.\n\n%s.\n\nTake the lock in the helper that writes, "+
					"before it registers anything that puts the row back, and in the "+
					"order internal/testdb declares.",
					where, shared.table, shared.lock, shared.table, shared.cost)
			}
		})
	}
}

// testPackage is one directory's test files: one test binary, so one
// process, whatever mix of `package x` and `package x_test` it holds.
type testPackage struct {
	rel      string
	files    []*ast.File
	ownDB    bool // some test file creates a database of its own
	sharedDB bool // some test file opens the shared one through internal/testdb
}

func testPackagesUnder(t *testing.T, root string) []testPackage {
	t.Helper()
	byDir := map[string]*testPackage{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// String literals and calls rather than the file's text, and the
		// difference is not pedantry: the first version of this check
		// matched whole files and immediately flagged
		// internal/invariants/dockerschema_test.go, which quotes
		// "INSERT INTO schema_version" inside a *comment* explaining a
		// past failure. A file that talks about a write is not a file
		// that performs one, and a check that cannot tell those apart
		// teaches people to add exceptions for prose.
		file, err := parser.ParseFile(token.NewFileSet(), path, body, 0)
		if err != nil {
			// Unparseable is somebody else's failure to report; here it
			// means "found nothing", which is the safe direction only
			// because the compiler will not accept the file either.
			return nil
		}
		dir := filepath.Dir(path)
		pkg := byDir[dir]
		if pkg == nil {
			rel, _ := filepath.Rel(root, dir)
			pkg = &testPackage{rel: filepath.ToSlash(rel)}
			byDir[dir] = pkg
		}
		pkg.files = append(pkg.files, file)
		if strings.Contains(string(body), "CREATE DATABASE ") {
			pkg.ownDB = true
		}
		for _, opener := range []string{"testdb.Pool(", "testdb.DSN(", "testdb.Admin("} {
			if strings.Contains(string(body), opener) {
				pkg.sharedDB = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]testPackage, 0, len(byDir))
	for _, pkg := range byDir {
		out = append(out, *pkg)
	}
	// Stable failure output; an error list that reshuffles between runs
	// is one people stop reading.
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// testFunc is what one declared function does, for the reach below.
type testFunc struct {
	writes bool            // a string literal in its body writes the row
	locks  bool            // it calls testdb.Lock with the row's key
	refs   map[string]bool // every name its body uses
}

// indexTestFuncs indexes a package's test files by the name a call would
// use: functions as themselves, methods as ".name" - a method call is
// resolved by name alone, whatever the receiver. Roots are the functions
// `go test` runs itself: every Test*, TestMain among them.
func indexTestFuncs(files []*ast.File, write *regexp.Regexp, lock string) (map[string]*testFunc, []string) {
	funcs := map[string]*testFunc{}
	var roots []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := fn.Name.Name
			if fn.Recv != nil {
				key = "." + key
			} else if strings.HasPrefix(key, "Test") {
				roots = append(roots, key)
			}
			info := funcs[key]
			if info == nil {
				info = &testFunc{refs: map[string]bool{}}
				funcs[key] = info
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind == token.STRING && write.MatchString(n.Value) {
						info.writes = true
					}
				case *ast.CallExpr:
					if isLockCall(n, lock) {
						info.locks = true
					}
				case *ast.SelectorExpr:
					info.refs["."+n.Sel.Name] = true
				case *ast.Ident:
					info.refs[n.Name] = true
				}
				return true
			})
		}
	}
	sort.Strings(roots)
	return funcs, roots
}

// isLockCall reports whether call is testdb.Lock(_, _, testdb.<lock>) -
// or, inside internal/testdb itself, Lock(_, _, <lock>).
func isLockCall(call *ast.CallExpr, lock string) bool {
	if len(call.Args) != 3 {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if fun.Sel.Name != "Lock" {
			return false
		}
	case *ast.Ident:
		if fun.Name != "Lock" {
			return false
		}
	default:
		return false
	}
	switch key := call.Args[2].(type) {
	case *ast.SelectorExpr:
		return key.Sel.Name == lock
	case *ast.Ident:
		return key.Name == lock
	}
	return false
}

// reachFrom lists root and every indexed function reachable from it.
func reachFrom(funcs map[string]*testFunc, root string) []string {
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for ref := range funcs[name].refs {
			if _, declared := funcs[ref]; declared && !seen[ref] {
				seen[ref] = true
				queue = append(queue, ref)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestALockCallCountsOnlyForItsOwnKey holds isLockCall's refusing half.
//
// Measured why it needs its own test: with the key comparison removed,
// the check above stayed green - and stayed green with the schema
// helpers' lock removed as well, because every writer in
// internal/panel/web builds its server through setupTestServer, which
// takes AccountsLock. Any lock would have satisfied a check that did not
// ask which.
func TestALockCallCountsOnlyForItsOwnKey(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"testdb.Lock(t, pool, testdb.SchemaVersionLock)", true},
		// Inside internal/testdb the names are unqualified.
		{"Lock(t, pool, SchemaVersionLock)", true},
		// Another suite's lock, which is what hid the key comparison.
		{"testdb.Lock(t, pool, testdb.AccountsLock)", false},
		// Naming the key is not taking it: internal/applier asks
		// pg_locks whether it holds the lock with exactly this argument.
		{"admin.QueryRow(ctx, q, int64(testdb.SchemaVersionLock))", false},
		{"testdb.Unlock(t, pool, testdb.SchemaVersionLock)", false},
	} {
		expr, err := parser.ParseExpr(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			t.Fatalf("%s is not a call", tc.src)
		}
		if got := isLockCall(call, "SchemaVersionLock"); got != tc.want {
			t.Errorf("isLockCall(%s) = %v, want %v", tc.src, got, tc.want)
		}
	}
}
