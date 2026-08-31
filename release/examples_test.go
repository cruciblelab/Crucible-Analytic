// The shipped example configurations parse.
//
// The cheapest test in this repository, and it was missing while
// beacon.example.toml had not been valid TOML for months:
//
//	toml: line 100 (last key "limits"): expected a top-level item to end
//	with a newline, comment, or EOF, but got 'd' instead
//
// A comment had lost its `#` when a paragraph was pasted into the middle
// of a sentence. install.sh copies these files verbatim into the
// operator's configuration directory, so every deployment that ran the
// beacon got "config error" and a process that would not start. Nothing
// caught it because nothing had ever started the beacon from its own
// example - the other three services had been, one way or another, and
// the beacon was the one nobody ran.
//
// Parsing is all this asserts. Whether the *values* are sensible is what
// each service's own config test is for; whether the file is even a file
// the parser will accept is this, and it is the failure that costs an
// operator an evening because the error names a line number in a comment.
//
// No build tag: four files and a TOML parser.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/applier"
	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/panel/web"
)

// TestEveryExampleConfigParses.
//
// Decoded into an empty struct with the parser the services use, not a
// third-party one and not a hand-written scan: what has to be true is
// that *this* parser accepts the file, which is the only claim worth
// making.
func TestEveryExampleConfigParses(t *testing.T) {
	root := repoRootFromWD(t)

	// Found from the tree rather than listed, so a fifth example config
	// added tomorrow is covered without anybody remembering this file.
	matches, err := filepath.Glob(filepath.Join(root, "*.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 5 {
		t.Fatalf("found %d example configs at the repository root, and this project has "+
			"five configured binaries - is the glob right?", len(matches))
	}

	for _, path := range matches {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var into map[string]any
			if _, err := toml.Decode(string(body), &into); err != nil {
				t.Fatalf("%s is not valid TOML: %v\n"+
					"install.sh copies this file into an operator's configuration directory unchanged, "+
					"so the service it belongs to will not start.", name, err)
			}
			if len(into) == 0 {
				t.Errorf("%s parsed to nothing; an example that configures nothing is not an example", name)
			}
		})
	}
}

// TestEveryCommentedSettingReachesItsField.
//
// The bug this exists for is not a typo. `secret_key` is a top-level
// setting in the panel's config, and its commented placeholder sat
// eighty lines below `[developer_gate]` - so uncommenting it, which is
// exactly what release/install.sh does after generating the key, put it
// inside that table. TOML is right and the file was wrong: every key
// after a header belongs to that header.
//
// What happened then is the shape worth remembering. install.sh
// generated a key, wrote it, read it back with a regex that does not
// know what a table is, and reported success. The panel started, did not
// find a secret_key, logged one line, and carried on - so the mail
// account could not be saved and nothing anywhere said why. Found by
// running the panel in a container and reading its first log line.
//
// So: uncomment each placeholder, decode into the struct the service
// actually uses, and require that the key was claimed by a field.
// BurntSushi's MetaData.Undecoded reports exactly the keys nothing
// claimed, which is the same question asked directly.
func TestEveryCommentedSettingReachesItsField(t *testing.T) {
	root := repoRootFromWD(t)
	checked := 0

	cases := []struct {
		file string
		into func() any
	}{
		{"config.example.toml", func() any { return new(collector.Config) }},
		{"beacon.example.toml", func() any { return new(beacon.Config) }},
		{"analytics-api.example.toml", func() any { return new(api.Config) }},
		{"panel.example.toml", func() any { return new(web.Config) }},
		{"upgrader.example.toml", func() any { return new(applier.Config) }},
	}

	// The list above is a hand list, and a hand list of files is a list
	// somebody forgets. So the tree is asked too: an example config with
	// no entry here is not merely unchecked, it is unchecked *silently*,
	// which is how a file gets shipped with a commented setting that
	// reaches no field.
	//
	// The sibling test globs and needs no list because it only parses.
	// This one has to know which struct each file is for, so the list
	// cannot be removed - only made unable to fall behind.
	listed := map[string]bool{}
	for _, c := range cases {
		listed[c.file] = true
	}
	onDisk, err := filepath.Glob(filepath.Join(root, "*.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range onDisk {
		if name := filepath.Base(path); !listed[name] {
			t.Errorf("%s is shipped and this test does not know which config struct "+
				"it belongs to, so nothing checks that its commented settings reach "+
				"a field. Add it below.", name)
		}
	}
	for name := range listed {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("this test lists %s, which is not at the repository root", name)
		}
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, c.file))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(body), "\n")

			for i, line := range lines {
				key, ok := commentedSetting(line)
				if !ok || insideACommentedBlock(lines, i) {
					continue
				}
				checked++

				edited := append([]string(nil), lines...)
				edited[i] = strings.TrimPrefix(strings.TrimSpace(line), "#")
				md, err := toml.Decode(strings.Join(edited, "\n"), c.into())
				if err != nil {
					t.Errorf("line %d (%s): uncommenting it makes the file invalid: %v", i+1, key, err)
					continue
				}
				for _, undecoded := range md.Undecoded() {
					if undecoded.String() == key || strings.HasSuffix(undecoded.String(), "."+key) {
						t.Errorf("line %d: %s reaches no field when uncommented - TOML reads it as %q, "+
							"which usually means the placeholder is below a [table] the setting does not belong to",
							i+1, key, undecoded.String())
					}
				}
			}
			t.Logf("%d commented settings checked", checked)
		})
	}
	// Across the files, not per file: analytics-api.example.toml's only
	// commented settings live inside a commented [[tokens]] block and are
	// correctly skipped, so a per-file floor would fail on a file that is
	// fine. What has to hold is that the pattern still finds something.
	if checked < 20 {
		t.Errorf("only %d commented settings found across the example configs; the pattern has probably stopped matching", checked)
	}
}

// commentedSetting reports the key on a `# key = value` line.
//
// The uncommented remainder has to be valid TOML on its own, which is
// what separates a placeholder from prose. These files explain
// themselves at length, and a sentence like
//
//	# level = "debug" is what makes the most common misconfiguration
//	# visible: with it on, security.log records every header …
//
// looks exactly like a setting for the first four words.
var settingLine = regexp.MustCompile(`^#\s*([A-Za-z_][A-Za-z0-9_]*)\s*=`)

func commentedSetting(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	m := settingLine.FindStringSubmatch(trimmed)
	if m == nil {
		return "", false
	}
	var probe map[string]any
	if _, err := toml.Decode(strings.TrimPrefix(trimmed, "#"), &probe); err != nil {
		return "", false
	}
	return m[1], true
}

// insideACommentedBlock reports whether a commented table header stands
// between this line and the last real one.
//
// The example configs show a second API token as a commented
// `# [[tokens]]` block. Uncommenting one line of it in isolation puts
// that key at the top level, where nothing claims it - a failure about
// the test's own editing rather than about the file.
func insideACommentedBlock(lines []string, at int) bool {
	for i := at - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "[") {
			return false // a real header: this line belongs to it
		}
		if strings.HasPrefix(trimmed, "#") && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "#")), "[") {
			return true
		}
	}
	return false
}
