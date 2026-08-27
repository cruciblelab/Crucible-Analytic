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
	"testing"

	"github.com/BurntSushi/toml"
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
	if len(matches) < 4 {
		t.Fatalf("found %d example configs at the repository root, and this project has four services - is the glob right?", len(matches))
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
