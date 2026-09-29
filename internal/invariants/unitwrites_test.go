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

	"github.com/cruciblelab/crucible-analytic/internal/applier"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

// What each systemd unit may write, asked of the unit files themselves
// and of the code that runs under them.
//
// # What went wrong
//
// The panel's version update (V5, 2026-09-04) never finished on a
// systemd install. The upgrader writes the new binaries into
// <prefix>/bin and rings the restart doorbell in /run/crucible-analytic;
// its unit, ProtectSystem=strict, listed neither as writable. The unit
// that DELETES the doorbell listed it; the unit that CREATES it did not.
// Measured with the real binary under a reproduction of the sandbox
// (NOTES, "V systemd altında"). Every test ran the installer in a
// t.TempDir() and the doorbell in-process, where everything is writable.
//
// # Why three rules and not one list
//
// A list of "the upgrader writes here" would be one more list to forget,
// exactly as the unit's ReadWritePaths was. So each rule is derived from
// the thing that makes it true:
//
//   - the doorbell: whichever binary constructs relupdate.Doorbell rings
//     it, so that binary's unit lists relupdate.DefaultDoorbellDir;
//   - the binaries: whichever binary constructs relupdate.Installer
//     replaces files under the configured prefix, so a drop-in for its
//     unit lists <default prefix>/bin - and the base unit does NOT,
//     because opening that directory is the operator's decision;
//   - root: nothing a root unit executes lies under a path any other
//     unit, or any drop-in, may write. The restarter's script lived in
//     the binary directory until the binary directory could be opened to
//     the upgrader; left there, opening it would have let the upgrader
//     choose what root runs on the next ring.

// unitFile is one parsed unit or drop-in.
type unitFile struct {
	path     string
	user     string   // "" means root: systemd's default, and restart's explicit one
	execs    []string // ExecStart* executables
	writable []string // ReadWritePaths, "-" prefixes stripped
}

var (
	unitUser     = regexp.MustCompile(`(?m)^User=(\S+)`)
	unitExec     = regexp.MustCompile(`(?m)^Exec(?:Start|StartPre|StartPost|Reload|Stop|StopPost)=[-@:+!]*(\S+)`)
	unitWritable = regexp.MustCompile(`(?m)^ReadWritePaths=(.*)$`)
)

func parseUnit(t *testing.T, path string) unitFile {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u := unitFile{path: path}
	if m := unitUser.FindSubmatch(body); m != nil {
		u.user = string(m[1])
	}
	for _, m := range unitExec.FindAllSubmatch(body, -1) {
		u.execs = append(u.execs, string(m[1]))
	}
	for _, m := range unitWritable.FindAllSubmatch(body, -1) {
		for _, p := range strings.Fields(string(m[1])) {
			u.writable = append(u.writable, strings.TrimPrefix(p, "-"))
		}
	}
	return u
}

// units reads release/systemd and release/dropins, keyed by unit name;
// a drop-in's writable paths are its unit's, which is how systemd merges
// them.
func units(t *testing.T, root string) (base map[string]unitFile, dropins map[string][]unitFile) {
	t.Helper()
	base = map[string]unitFile{}
	files, err := filepath.Glob(filepath.Join(root, "release", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		base[filepath.Base(f)] = parseUnit(t, f)
	}
	dropins = map[string][]unitFile{}
	dirs, err := filepath.Glob(filepath.Join(root, "release", "dropins", "*.d"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		unit := strings.TrimSuffix(filepath.Base(d), ".d")
		if _, ok := base[unit]; !ok {
			t.Errorf("release/dropins/%s names %s, and release/systemd has no such unit; "+
				"systemd would read it for nothing", filepath.Base(d), unit)
			continue
		}
		confs, err := filepath.Glob(filepath.Join(d, "*.conf"))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range confs {
			dropins[unit] = append(dropins[unit], parseUnit(t, c))
		}
	}
	if len(base) < 5 {
		t.Fatalf("read %d service units; the directory moved or the glob stopped matching", len(base))
	}
	return base, dropins
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// unitRunning finds the unit whose ExecStart runs cmd/<name>'s binary.
func unitRunning(t *testing.T, base map[string]unitFile, name string) string {
	t.Helper()
	var found []string
	for unit, u := range base {
		for _, e := range u.execs {
			if filepath.Base(e) == name {
				found = append(found, unit)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("cmd/%s is run by %d units (%v); want exactly one", name, len(found), found)
	}
	return found[0]
}

// constructors lists the cmd/<name> whose Go files build a composite
// literal of pkg.typ - read from the syntax tree, so a comment naming
// the type is not a construction.
func constructors(t *testing.T, root, pkg, typ string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(root, "cmd", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		builds := false
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == typ {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
						builds = true
					}
				}
				return true
			})
		}
		if builds {
			out = append(out, filepath.Base(dir))
		}
	}
	sort.Strings(out)
	return out
}

func TestTheUnitThatRingsTheDoorbellMayWriteIt(t *testing.T) {
	root := repoRoot(t)
	base, _ := units(t, root)
	ringers := constructors(t, root, "relupdate", "Doorbell")
	if len(ringers) == 0 {
		t.Fatal("no cmd/*/main.go constructs relupdate.Doorbell; the doorbell moved, " +
			"and this test would pass by checking nothing")
	}
	for _, name := range ringers {
		unit := unitRunning(t, base, name)
		found := false
		for _, w := range base[unit].writable {
			if w == relupdate.DefaultDoorbellDir {
				found = true
			}
		}
		if !found {
			t.Errorf("cmd/%s rings the doorbell in %s, and %s does not list it in "+
				"ReadWritePaths. Under ProtectSystem=strict every ring fails with "+
				"\"read-only file system\" - measured, 2026-09-29",
				name, relupdate.DefaultDoorbellDir, unit)
		}
	}
}

func TestTheBinaryDirectoryOpensOnlyThroughTheOptIn(t *testing.T) {
	root := repoRoot(t)
	base, dropins := units(t, root)
	bin := filepath.Join(applier.ReleaseConfig{}.InstallPrefix(), "bin")

	installers := constructors(t, root, "relupdate", "Installer")
	if len(installers) == 0 {
		t.Fatal("no cmd/*/main.go constructs relupdate.Installer; the installer moved, " +
			"and this test would pass by checking nothing")
	}
	for _, name := range installers {
		unit := unitRunning(t, base, name)
		for _, w := range base[unit].writable {
			if under(bin, w) {
				t.Errorf("%s lists %s writable in the unit itself. Opening the binary "+
					"directory to the upgrader is the operator's decision, taken with the "+
					"drop-in in release/dropins/%s.d; in the base unit it would be taken "+
					"for every install", unit, w, unit)
			}
		}
		opened := false
		for _, d := range dropins[unit] {
			for _, w := range d.writable {
				if w == bin {
					opened = true
				}
			}
		}
		if !opened {
			t.Errorf("cmd/%s replaces the binaries under %s and no drop-in in "+
				"release/dropins/%s.d lists that directory writable, so updates from the "+
				"panel can never finish on a systemd install", name, bin, unit)
		}
	}
}

func TestRootNeverRunsWhatAServiceAccountMayWrite(t *testing.T) {
	root := repoRoot(t)
	base, dropins := units(t, root)

	type writer struct{ unit, path string }
	var writers []writer
	for unit, u := range base {
		if u.user == "" || u.user == "root" {
			continue
		}
		for _, w := range u.writable {
			writers = append(writers, writer{unit, w})
		}
		for _, d := range dropins[unit] {
			for _, w := range d.writable {
				writers = append(writers, writer{unit + " (" + filepath.Base(d.path) + ")", w})
			}
		}
	}
	if len(writers) == 0 {
		t.Fatal("no service unit lists a writable path; the parse is not reading the files")
	}

	roots := 0
	for unit, u := range base {
		if u.user != "" && u.user != "root" {
			continue
		}
		roots++
		if len(u.execs) == 0 {
			t.Errorf("%s runs as root and has no Exec line this test can read", unit)
		}
		for _, e := range u.execs {
			for _, w := range writers {
				if under(e, w.path) {
					t.Errorf("%s runs %s as root, and %s may write %s. Whoever holds that "+
						"account chooses what root runs next", unit, e, w.unit, w.path)
				}
			}
		}
	}
	if roots == 0 {
		t.Fatal("no unit runs as root; the restarter moved, and this test would pass by checking nothing")
	}
}
