package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// lockWitness is where one "A before B" was seen.
type lockWitness struct{ pkg, test string }

// TestNoTwoSuitesTakeTheSameLocksInOppositeOrders.
//
// testdb.Lock holds a Postgres advisory lock until the test ends, and
// `go test ./...` runs packages side by side. A test in one package that
// holds A and waits for B, while a test in another holds B and waits for
// A, waits forever - and what CI shows is two packages killed at ten
// minutes, a goroutine dump, and no sentence naming either lock.
//
// That is not hypothetical. On 2026-09-27 I moved
// TestTheRefreshButtonWorksFromThePage's two queue locks ahead of its
// server, following internal/testdb's written rule ("take them in the
// order they are declared here"). The server's setup takes AccountsLock,
// and internal/panel's fetch-log tests had taken AccountsLock first and
// FetchLogLock second for months; the rule had never described them.
// CI 424 and 425 went red on the second integration pass, both packages
// at 600 seconds - internal/panel parked in testdb.Lock(FetchLogLock) for
// 8m28s, internal/panel/web waiting for AccountsLock underneath. Two
// local gates had passed: a deadlock needs the two tests to overlap.
//
// So the order is read from the code, and the written rule is checked
// against it. Each test's locks are collected in the order the test takes
// them, through every helper in its package. Two things fail:
//
//   - a pair taken in both orders anywhere, naming the tests on each side;
//   - a pair taken against the order internal/testdb declares its keys in,
//     which is the rule its comments state. That one also fails a single
//     test with nobody on the other side yet, and it covers cycles of any
//     length: a cycle cannot follow one declared order.
//
// Stricter than a deadlock, on purpose. Two tests that both take a third
// lock first cannot deadlock on the pair after it, because the first lock
// admits one of them at a time - the refresh test's old order was that
// case. But that protection is invisible at the pair: it holds until
// someone reorders the outer lock, which is exactly what went wrong.
//
// What this does not see: a lock taken inside production code (the
// applier's schema lock), and a lock taken in a closure handed to a
// helper that takes locks of its own - there the order depends on when
// the helper calls the closure, so that is reported rather than guessed.
func TestNoTwoSuitesTakeTheSameLocksInOppositeOrders(t *testing.T) {
	root := repoRootFromInvariants(t)
	edges := map[[2]string][]lockWitness{}
	takers := 0
	for _, pkg := range testPackagesUnder(t, root) {
		seqs, problems := lockSequences(pkg.files)
		for _, p := range problems {
			t.Errorf("%s: %s", pkg.rel, p)
		}
		for test, seq := range seqs {
			if len(seq) > 0 {
				takers++
			}
			for pair := range lockEdges(seq) {
				edges[pair] = append(edges[pair], lockWitness{pkg.rel, test})
			}
		}
	}
	if takers < 10 {
		t.Fatalf("only %d tests take a lock, where this repository has had hundreds; "+
			"the call this check looks for has probably changed shape, and it is "+
			"now checking nothing", takers)
	}

	for _, pair := range invertedPairs(edges) {
		a, b := pair[0], pair[1]
		t.Errorf("%s and %s are taken in both orders.\n\n"+
			"%s before %s:\n    %s\n\n%s before %s:\n    %s\n\n"+
			"Two of these in different packages, overlapping, wait for each other "+
			"until go test kills both at ten minutes. Take them in the order "+
			"internal/testdb declares them; the lock a helper takes counts where "+
			"the helper is called.",
			a, b, a, b, lockWitnesses(edges[[2]string{a, b}]), b, a, lockWitnesses(edges[[2]string{b, a}]))
	}

	unknown, against := outOfOrder(edges, declaredLockOrder(t, root))
	for _, key := range unknown {
		t.Errorf("%s is not a lock internal/testdb declares, so its place in the order "+
			"cannot be checked", key)
	}
	for _, pair := range against {
		a, b := pair[0], pair[1]
		t.Errorf("%s is taken before %s, and internal/testdb declares %s first.\n\n"+
			"%s before %s:\n    %s\n\n"+
			"Take them in the declared order - or, if every suite really takes them "+
			"this way, move the declaration and say why in its # Ordering note.",
			a, b, b, a, b, lockWitnesses(edges[pair]))
	}

	if !t.Failed() {
		// The order the suites use today, for whoever adds the next lock.
		var pairs []string
		for pair := range edges {
			pairs = append(pairs, pair[0]+" before "+pair[1])
		}
		sort.Strings(pairs)
		t.Logf("%d tests take locks; the pairs they hold together:\n  %s",
			takers, strings.Join(pairs, "\n  "))
	}
}

// lockEdges is every "A before B" one test's sequence holds: a lock is
// held until the test ends, so each is held when every later one is
// taken.
func lockEdges(seq []string) map[[2]string]bool {
	out := map[[2]string]bool{}
	for i := range seq {
		for j := i + 1; j < len(seq); j++ {
			out[[2]string{seq[i], seq[j]}] = true
		}
	}
	return out
}

// invertedPairs lists the pairs taken in both orders, each once.
func invertedPairs(edges map[[2]string][]lockWitness) [][2]string {
	var out [][2]string
	for pair := range edges {
		if pair[0] < pair[1] {
			if _, back := edges[[2]string{pair[1], pair[0]}]; back {
				out = append(out, pair)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0]+out[i][1] < out[j][0]+out[j][1] })
	return out
}

// outOfOrder lists the keys the declared order does not know, and the
// pairs taken against it.
func outOfOrder(edges map[[2]string][]lockWitness, rank map[string]int) ([]string, [][2]string) {
	unknown := map[string]bool{}
	var against [][2]string
	for pair := range edges {
		ra, oka := rank[pair[0]]
		rb, okb := rank[pair[1]]
		if !oka {
			unknown[pair[0]] = true
		}
		if !okb {
			unknown[pair[1]] = true
		}
		if oka && okb && ra > rb {
			against = append(against, pair)
		}
	}
	keys := make([]string, 0, len(unknown))
	for key := range unknown {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sort.Slice(against, func(i, j int) bool {
		return against[i][0]+against[i][1] < against[j][0]+against[j][1]
	})
	return keys, against
}

// lockWitnesses lists where a pair was seen, one test to a line.
func lockWitnesses(w []lockWitness) string {
	out := make([]string, 0, len(w))
	for _, one := range w {
		out = append(out, one.pkg+": "+one.test)
	}
	sort.Strings(out)
	return strings.Join(out, "\n    ")
}

// declaredLockOrder reads internal/testdb's lock constants in the order
// the file declares them.
func declaredLockOrder(t *testing.T, root string) map[string]int {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(),
		filepath.Join(root, "internal", "testdb", "testdb.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	rank := map[string]int{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, id := range spec.(*ast.ValueSpec).Names {
				if strings.HasSuffix(id.Name, "Lock") {
					rank[id.Name] = len(rank)
				}
			}
		}
	}
	if len(rank) < 5 {
		t.Fatalf("internal/testdb declares %d lock constants; this repository has had "+
			"eleven, so the file has probably moved and the order is being read from "+
			"nothing", len(rank))
	}
	return rank
}

// lockSequences lists, for each Test function, the locks it takes in the
// order it takes them, following calls into the package's own functions.
// Problems are lock calls whose key this check cannot name and closures
// whose place in the order it cannot know.
func lockSequences(files []*ast.File) (map[string][]string, []string) {
	decls := map[string][]*ast.FuncDecl{}
	var roots []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := fn.Name.Name
			if fn.Recv != nil {
				// A method is resolved by name alone, as in indexTestFuncs.
				key = "." + key
			} else if strings.HasPrefix(key, "Test") {
				roots = append(roots, key)
			}
			decls[key] = append(decls[key], fn)
		}
	}

	problems := map[string]bool{}
	memo := map[string][]string{}
	visiting := map[string]bool{}
	current := ""
	var seqOf func(key string) []string
	var walk func(n ast.Node, out *[]string)

	calleeKey := func(call *ast.CallExpr) string {
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if _, ok := decls[fun.Name]; ok {
				return fun.Name
			}
		case *ast.SelectorExpr:
			if _, ok := decls["."+fun.Sel.Name]; ok {
				return "." + fun.Sel.Name
			}
		}
		return ""
	}
	seqOf = func(key string) []string {
		if seq, done := memo[key]; done {
			return seq
		}
		if visiting[key] {
			// Recursion adds nothing the first pass did not.
			return nil
		}
		visiting[key] = true
		outer := current
		current = strings.TrimPrefix(key, ".")
		var seq []string
		for _, fn := range decls[key] {
			walk(fn.Body, &seq)
		}
		current = outer
		visiting[key] = false
		memo[key] = seq
		return seq
	}
	// walk appends locks in execution order: a call's arguments run
	// before the call does, so they are walked first.
	walk = func(n ast.Node, out *[]string) {
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			walk(call.Fun, out)
			for _, arg := range call.Args {
				walk(arg, out)
			}
			if isAnyLockCall(call) {
				if key, named := lockKey(call); named {
					*out = append(*out, key)
				} else {
					problems[current+": a testdb.Lock whose key is not a testdb constant, "+
						"so its place in the order cannot be read"] = true
				}
				return false
			}
			if callee := calleeKey(call); callee != "" {
				inner := seqOf(callee)
				if len(inner) > 0 {
					for _, arg := range call.Args {
						if lit, ok := arg.(*ast.FuncLit); ok && takesALock(lit) {
							problems[current+": a closure that takes a lock is handed to "+
								strings.TrimPrefix(callee, ".")+", which takes locks of its own; "+
								"which comes first depends on when it calls the closure. "+
								"Take the lock in the helper, in the order it needs"] = true
						}
					}
				}
				*out = append(*out, inner...)
			}
			return false
		})
	}

	seqs := map[string][]string{}
	for _, root := range roots {
		seen := map[string]bool{}
		var seq []string
		for _, lock := range seqOf(root) {
			// testdb.Lock refuses a second hold of the same key, so a
			// repeat here is a path the test cannot take twice.
			if !seen[lock] {
				seen[lock] = true
				seq = append(seq, lock)
			}
		}
		seqs[root] = seq
	}
	out := make([]string, 0, len(problems))
	for p := range problems {
		out = append(out, p)
	}
	sort.Strings(out)
	return seqs, out
}

// isAnyLockCall reports whether call is testdb.Lock(_, _, _), or Lock
// with three arguments inside internal/testdb.
func isAnyLockCall(call *ast.CallExpr) bool {
	if len(call.Args) != 3 {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		return ok && pkg.Name == "testdb" && fun.Sel.Name == "Lock"
	case *ast.Ident:
		return fun.Name == "Lock"
	}
	return false
}

// lockKey names the key of a lock call: testdb.X, or X inside testdb.
func lockKey(call *ast.CallExpr) (string, bool) {
	switch key := call.Args[2].(type) {
	case *ast.SelectorExpr:
		if pkg, ok := key.X.(*ast.Ident); ok && pkg.Name == "testdb" && strings.HasSuffix(key.Sel.Name, "Lock") {
			return key.Sel.Name, true
		}
	case *ast.Ident:
		if strings.HasSuffix(key.Name, "Lock") {
			return key.Name, true
		}
	}
	return "", false
}

// takesALock reports whether n contains a lock call anywhere.
func takesALock(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isAnyLockCall(call) {
			found = true
		}
		return !found
	})
	return found
}

// TestALockSequenceIsReadInTheOrderTheLocksAreTaken holds lockSequences
// to the shapes the repository uses, each one a way to read the order
// wrong.
func TestALockSequenceIsReadInTheOrderTheLocksAreTaken(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      string
		problem   string
		// anyOrder: the sequence is not the point, and not knowable.
		anyOrder bool
	}{
		{name: "in the test itself", want: "AccountsLock FetchLogLock", src: `
			func TestX(t *testing.T) {
				testdb.Lock(t, a, testdb.AccountsLock)
				testdb.Lock(t, a, testdb.FetchLogLock)
			}`},
		{name: "through a helper, where it is called", want: "AccountsLock FetchLogLock RefreshQueueLock", src: `
			func server(t *testing.T) { testdb.Lock(t, a, testdb.AccountsLock) }
			func TestX(t *testing.T) {
				server(t)
				testdb.Lock(t, a, testdb.FetchLogLock)
				testdb.Lock(t, a, testdb.RefreshQueueLock)
			}`},
		{name: "a helper's helper", want: "AccountsLock HeartbeatLock", src: `
			func setup(t *testing.T) { testdb.Lock(t, a, testdb.AccountsLock) }
			func server(t *testing.T) { setup(t); testdb.Lock(t, a, testdb.HeartbeatLock) }
			func TestX(t *testing.T) { server(t) }`},
		{name: "an argument runs before the call it is passed to", want: "AccountsLock FetchLogLock", src: `
			func store(t *testing.T) int { testdb.Lock(t, a, testdb.AccountsLock); return 0 }
			func fetchLog(t *testing.T, _ int) { testdb.Lock(t, a, testdb.FetchLogLock) }
			func TestX(t *testing.T) { fetchLog(t, store(t)) }`},
		{name: "a subtest runs inside its parent", want: "AccountsLock HeartbeatLock", src: `
			func TestX(t *testing.T) {
				testdb.Lock(t, a, testdb.AccountsLock)
				t.Run("sub", func(t *testing.T) { testdb.Lock(t, a, testdb.HeartbeatLock) })
			}`},
		{name: "a method, by name", want: "ReleaseQueueLock HeartbeatLock", src: `
			func (q *queue) open(t *testing.T) { testdb.Lock(t, a, testdb.ReleaseQueueLock) }
			func TestX(t *testing.T) {
				q.open(t)
				testdb.Lock(t, a, testdb.HeartbeatLock)
			}`},
		{name: "a helper called twice counts once", want: "AccountsLock FetchLogLock", src: `
			func store(t *testing.T) { testdb.Lock(t, a, testdb.AccountsLock) }
			func TestX(t *testing.T) {
				store(t)
				testdb.Lock(t, a, testdb.FetchLogLock)
				store(t)
			}`},
		{name: "recursion ends", want: "AccountsLock", src: `
			func again(t *testing.T) { testdb.Lock(t, a, testdb.AccountsLock); again(t) }
			func TestX(t *testing.T) { again(t) }`},
		{name: "a key that is not a constant", want: "",
			problem: "a testdb.Lock whose key is not a testdb constant", src: `
			func TestX(t *testing.T, key int64) { testdb.Lock(t, a, key) }`},
		{name: "a lock in a closure handed to a locking helper", anyOrder: true,
			problem: "a closure that takes a lock is handed to server", src: `
			func server(t *testing.T, tweak func()) { testdb.Lock(t, a, testdb.AccountsLock); tweak() }
			func TestX(t *testing.T) {
				server(t, func() { testdb.Lock(t, a, testdb.HeartbeatLock) })
			}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x_test.go", "package x\n"+tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			seqs, problems := lockSequences([]*ast.File{file})
			if got := strings.Join(seqs["TestX"], " "); !tc.anyOrder && got != tc.want {
				t.Errorf("sequence = %q, want %q", got, tc.want)
			}
			joined := strings.Join(problems, "\n")
			if tc.problem == "" && joined != "" {
				t.Errorf("unexpected problems: %s", joined)
			}
			if tc.problem != "" && !strings.Contains(joined, tc.problem) {
				t.Errorf("problems = %q, want one containing %q", joined, tc.problem)
			}
		})
	}
}

// TestBothChecksCanFail feeds the two checks a pair taken both ways, the
// shape CI 424 had, so each is known to be able to say no.
func TestBothChecksCanFail(t *testing.T) {
	edges := map[[2]string][]lockWitness{}
	for pair := range lockEdges([]string{"AccountsLock", "FetchLogLock"}) {
		edges[pair] = append(edges[pair], lockWitness{"internal/panel", "TestRead"})
	}
	for pair := range lockEdges([]string{"FetchLogLock", "RefreshQueueLock", "AccountsLock"}) {
		edges[pair] = append(edges[pair], lockWitness{"internal/panel/web", "TestPage"})
	}

	inverted := invertedPairs(edges)
	if len(inverted) != 1 || inverted[0] != [2]string{"AccountsLock", "FetchLogLock"} {
		t.Errorf("invertedPairs = %v, want only AccountsLock/FetchLogLock", inverted)
	}

	rank := map[string]int{"AccountsLock": 0, "FetchLogLock": 1, "RefreshQueueLock": 2}
	unknown, against := outOfOrder(edges, rank)
	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none", unknown)
	}
	want := [][2]string{{"FetchLogLock", "AccountsLock"}, {"RefreshQueueLock", "AccountsLock"}}
	if len(against) != len(want) || against[0] != want[0] || against[1] != want[1] {
		t.Errorf("against the declared order = %v, want %v", against, want)
	}

	// A key the declared order has never heard of is not in order.
	unknown, _ = outOfOrder(map[[2]string][]lockWitness{{"AccountsLock", "NewLock"}: nil}, rank)
	if len(unknown) != 1 || unknown[0] != "NewLock" {
		t.Errorf("unknown = %v, want [NewLock]", unknown)
	}
}
