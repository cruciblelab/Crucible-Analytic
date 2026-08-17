package ui

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCatalog(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if cat.Len() == 0 {
		t.Fatal("catalog is empty")
	}
	if got := cat.T("uygulama.ad"); got != "Crucible Analytic" {
		t.Errorf("uygulama.ad = %q", got)
	}
}

func TestMissingKeyIsVisibleRatherThanBlank(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	got := cat.T("bir.yok.anahtar")
	if got == "" {
		t.Fatal("a missing key rendered as empty; that is invisible on the page")
	}
	if !strings.HasPrefix(got, markPrefix) || !strings.HasSuffix(got, markSuffix) {
		t.Fatalf("missing key rendered as %q, want it wrapped in the marker", got)
	}
	if !strings.Contains(got, "bir.yok.anahtar") {
		t.Fatalf("missing key marker %q does not name the key, so nobody can fix it", got)
	}
}

func TestFlattenRefusesWhatWouldRenderAsNothing(t *testing.T) {
	cases := []struct {
		name string
		tree map[string]any
		want string
	}{
		{
			name: "empty string",
			tree: map[string]any{"a": map[string]any{"b": "   "}},
			want: "empty",
		},
		{
			name: "not text",
			tree: map[string]any{"a": map[string]any{"b": int64(3)}},
			want: "only text is allowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]string{}
			err := flatten("", tc.tree, out)
			if err == nil {
				t.Fatalf("flatten accepted %v", tc.tree)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTfFillsVerbs(t *testing.T) {
	cat := &Catalog{entries: map[string]string{"x": "%d dakika önce"}}
	if got := cat.Tf("x", 5); got != "5 dakika önce" {
		t.Errorf("Tf = %q", got)
	}
	if got := cat.Tf("yok", 5); !strings.Contains(got, "yok") {
		t.Errorf("Tf on a missing key = %q", got)
	}
}

// TestTemplatesNameNoUnknownKey is the check that runs at startup,
// asserted here so a broken template fails in CI rather than at boot on
// a customer's machine.
func TestTemplatesNameNoUnknownKey(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	_, trees, err := parsePages(fixtureFuncs(cat))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTemplateKeys(trees, cat); err != nil {
		t.Fatal(err)
	}
}

// TestNewRefusesATemplateKeyThatDoesNotExist proves the startup check
// actually stops the binary, rather than being a function nobody calls.
func TestNewRefusesATemplateKeyThatDoesNotExist(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	// A catalog missing one key the layout names.
	delete(cat.entries, "gezinme.atla")
	assets, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cat, assets, nil); err == nil {
		t.Fatal("New accepted a catalog missing a key the templates use")
	} else if !strings.Contains(err.Error(), "gezinme.atla") {
		t.Fatalf("error %q does not name the missing key", err)
	}
}

// TestEveryErrorStatusHasItsOwnWording covers the keys that are built
// at runtime and are therefore invisible to the template walk.
func TestEveryErrorStatusHasItsOwnWording(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range mappedErrorStatuses {
		titleKey, bodyKey := errorKeys(status)
		if !cat.Has(titleKey) || !cat.Has(bodyKey) {
			t.Errorf("status %d maps to %s/%s which the catalog does not have", status, titleKey, bodyKey)
		}
	}
	// An unmapped status must fall back rather than produce a marker in
	// front of the reader.
	titleKey, bodyKey := errorKeys(http.StatusTeapot)
	if titleKey != "hata.500.baslik" || bodyKey != "hata.500.govde" {
		t.Errorf("unmapped status fell back to %s/%s", titleKey, bodyKey)
	}
}

// mappedErrorStatuses mirrors the switch in errorKeys. Kept as a
// separate list on purpose: if somebody adds a case there and not here
// the test below notices the catalog entry nothing references, and if
// they add it here and not there this test fails on the missing
// wording. Either mistake surfaces.
var mappedErrorStatuses = []int{
	http.StatusBadRequest,
	http.StatusForbidden,
	http.StatusNotFound,
	http.StatusMethodNotAllowed,
	statusCSRFExpired,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
}

// TestNoDeadCatalogEntries runs the completeness check the other way.
//
// A catalog that only ever grows becomes a file nobody trusts: the
// reader cannot tell which sentences are on a page and which were left
// behind by a rewrite two phases ago. So a key that no template and no
// Go source names is an error, and the fix is to delete it - or to
// finish the page that was supposed to use it.
func TestNoDeadCatalogEntries(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	_, trees, err := parsePages(fixtureFuncs(cat))
	if err != nil {
		t.Fatal(err)
	}

	used := map[string]bool{}
	for _, key := range templateKeys(trees) {
		used[key] = true
	}
	for _, status := range mappedErrorStatuses {
		titleKey, bodyKey := errorKeys(status)
		used[titleKey], used[bodyKey] = true, true
	}

	sources, err := goSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range cat.Keys() {
		if used[key] {
			continue
		}
		quoted := `"` + key + `"`
		for _, body := range sources {
			if strings.Contains(body, quoted) {
				used[key] = true
				break
			}
		}
	}

	var dead []string
	for _, key := range cat.Keys() {
		if !used[key] {
			dead = append(dead, key)
		}
	}
	if len(dead) > 0 {
		t.Fatalf("catalog keys nothing uses:\n  %s\n\nDelete them, or finish the page that was going to show them.",
			strings.Join(dead, "\n  "))
	}
}

// goSources reads every .go file in the module, so a key referenced
// from a handler counts as used.
func goSources() (map[string]string, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".go" {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = string(body)
		return nil
	})
	return out, err
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fs.ErrNotExist
		}
		dir = parent
	}
}

// fixtureFuncs is the same func map the renderer builds, for tests that
// need to parse without constructing one.
func fixtureFuncs(cat *Catalog) map[string]any {
	assets, err := LoadAssets()
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"t":     cat.T,
		"tf":    cat.Tf,
		"asset": assets.URL,
	}
}
