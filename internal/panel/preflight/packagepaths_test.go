package preflight

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"reflect"
)

// Every command the setup wizard prints names something the customer's
// release package has (W4, item 3).
//
// Before W4 they named this repository: `psql -f internal/panel/schema.sql`,
// `for f in internal/*/schema.sql`, `go run ./cmd/devpass`, "README'deki
// CREATE ROLE blokları", `deploy/systemd/*.service`, a config file called
// crucible.toml. A customer has none of those. They have a tarball with
// bin/, schema/, systemd/ and release/install.sh - and an installed
// system with the binaries in one directory and the config files in
// another, which is what the panel now puts into the commands.
//
// What the package holds is read out of release/build.sh, the script that
// makes it, rather than listed here: a file the commands name and the
// build stops shipping turns this red.

// wizardCommands is every command text the wizard can print without a
// database: the manual steps and the fixes that are built from paths.
func wizardCommands(cfg Config) []string {
	var out []string
	for _, step := range ManualSteps(cfg) {
		out = append(out, step.Command)
	}
	out = append(out, devpassCommand(cfg), ipKeyCommand(cfg), botDataCommand(cfg),
		logDirCommand(cfg), packageInstall)
	return out
}

// packagedFiles reads release/build.sh for what it puts into the package,
// as package-relative paths: bin/<name> for each binary it builds, and
// release/<file> for each script and SQL file it installs.
func packagedFiles(t *testing.T) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "release", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	text := string(body)

	loop := regexp.MustCompile(`(?m)^for b in ([a-z -]+); do`).FindStringSubmatch(text)
	if loop == nil {
		t.Fatal("release/build.sh has no `for b in ...` binary loop; this test cannot tell " +
			"which binaries the package holds")
	}
	for _, b := range strings.Fields(loop[1]) {
		out["bin/"+b] = true
	}

	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, `${STAGE}/release/`) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "release/") {
				continue
			}
			matches, err := filepath.Glob(filepath.Join(root, field))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range matches {
				rel, _ := filepath.Rel(root, m)
				out[filepath.ToSlash(rel)] = true
			}
		}
	}
	if !out["release/install.sh"] {
		t.Fatalf("the reading of release/build.sh found no release/install.sh (%v); it is "+
			"what the wizard sends people to, so the reading is wrong", out)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package")
		}
		dir = parent
	}
}

// sourceTree is how a path into this repository looks in a sentence: a
// directory the package does not have, or a Go toolchain command.
var sourceTree = regexp.MustCompile(`go run |(^|[\s"'(:])(internal|cmd|deploy)/|crucible\.toml|README'deki`)

var packageRelative = regexp.MustCompile(`\./(release/[A-Za-z0-9_./-]+)|\b(release/sql/[a-z]+\.sql)`)

func TestEveryWizardCommandNamesThePackage(t *testing.T) {
	packaged := packagedFiles(t)
	cfg := Config{BinDir: "/srv/ca/bin", ConfigDir: "/srv/ca/etc", LogDir: "/srv/ca/log"}

	for _, cmd := range wizardCommands(cfg) {
		if m := sourceTree.FindString(cmd); m != "" {
			t.Errorf("a wizard command names the source tree (%q): %s", m, cmd)
		}
		for _, m := range packageRelative.FindAllStringSubmatch(cmd, -1) {
			path := m[1] + m[2]
			if !packaged[path] {
				t.Errorf("a wizard command names %s, which release/build.sh does not put into "+
					"the package: %s", path, cmd)
			}
		}
	}

	// The binaries and config files are named where this deployment has
	// them, not where a default would put them - and asked of each command
	// on its own. A first version asked whether some command named each
	// path, and two commands share the devpass binary and collector.toml,
	// so a command that dropped its paths was covered by its neighbour.
	for name, c := range map[string]struct {
		text string
		want []string
	}{
		"devpass":  {devpassCommand(cfg), []string{"/srv/ca/bin/devpass", "/srv/ca/etc/panel.toml"}},
		"ipkey":    {ipKeyCommand(cfg), []string{"/srv/ca/bin/devpass -ipkey", "/srv/ca/etc/collector.toml", "/srv/ca/etc/beacon.toml"}},
		"bot data": {botDataCommand(cfg), []string{"/srv/ca/bin/collector", "-config /srv/ca/etc/collector.toml"}},
		"log dir":  {logDirCommand(cfg), []string{"/srv/ca/log"}},
	} {
		for _, want := range c.want {
			if !strings.Contains(c.text, want) {
				t.Errorf("the %s command does not name %s: %s", name, want, c.text)
			}
		}
	}
	for _, binary := range []string{"devpass", "collector"} {
		if !packaged["bin/"+binary] {
			t.Errorf("the wizard runs bin/%s and the package does not build it", binary)
		}
	}
}

// The keys the bootstrap step lists are the collector's keys, read off its
// configuration type: the step used to promise "sekiz anahtar", list six,
// and name crucible.toml.
func TestTheBootstrapKeysAreTheCollectorsKeys(t *testing.T) {
	for _, key := range bootstrapKeys {
		typ := reflect.TypeOf(collector.Config{})
		for _, part := range strings.Split(key, ".") {
			field, ok := fieldByTag(typ, part)
			if !ok {
				t.Errorf("the bootstrap step names %q, which collector.Config does not have "+
					"(no field tagged %q in %s)", key, part, typ)
				break
			}
			typ = field.Type
		}
	}
}

func fieldByTag(typ reflect.Type, tag string) (reflect.StructField, bool) {
	if typ.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	for i := 0; i < typ.NumField(); i++ {
		if strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0] == tag {
			return typ.Field(i), true
		}
	}
	return reflect.StructField{}, false
}
