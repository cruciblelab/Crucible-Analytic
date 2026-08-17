package ui

import (
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCatalogs(t *testing.T) *Catalogs {
	t.Helper()
	cats, err := LoadCatalogs()
	if err != nil {
		t.Fatalf("LoadCatalogs: %v", err)
	}
	return cats
}

func TestLoadCatalogs(t *testing.T) {
	cats := testCatalogs(t)
	if len(cats.Languages()) < 2 {
		t.Fatalf("only %d language pack(s) loaded; the machinery is untested with one", len(cats.Languages()))
	}
	if cats.Base().Code != BaseLanguageCode {
		t.Errorf("base language is %q", cats.Base().Code)
	}
	// The base must come first, because language.Matcher treats the
	// first tag as the default for anything it cannot serve.
	if cats.Languages()[0] != cats.Base() {
		t.Error("the base language is not first; an unservable Accept-Language would land somewhere else")
	}
	for _, lang := range cats.Languages() {
		if lang.Len() == 0 {
			t.Errorf("%s defines no messages", lang.Code)
		}
		if lang.Name == "" {
			t.Errorf("%s has no endonym", lang.Code)
		}
		if got := lang.T("uygulama.ad"); got != "Crucible Analytic" {
			t.Errorf("%s: uygulama.ad = %q", lang.Code, got)
		}
	}
}

// TestEveryPackIsComplete is where an untranslated string is supposed
// to hurt: in a build, not on a customer's server. The renderer only
// warns, so this is the check that actually stops it shipping.
func TestEveryPackIsComplete(t *testing.T) {
	cats := testCatalogs(t)
	for code, missing := range cats.Gaps() {
		if len(missing) > 0 {
			t.Errorf("messages/%s.toml is missing %d key(s):\n  %s",
				code, len(missing), strings.Join(missing, "\n  "))
		}
	}
	// And the reverse: a key only a translation defines is either a typo
	// or something that should have gone into the base pack first.
	base := cats.Base()
	for _, lang := range cats.Languages() {
		if lang == base {
			continue
		}
		for _, key := range lang.Keys() {
			if !base.Has(key) {
				t.Errorf("messages/%s.toml defines %q, which the base pack does not", lang.Code, key)
			}
		}
	}
}

func TestMissingKeyIsVisibleRatherThanBlank(t *testing.T) {
	cat := testCatalogs(t).Base()
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
			want: "text or a table of plural forms",
		},
		{
			name: "plural forms with no other",
			tree: map[string]any{"a": map[string]any{"one": "bir"}},
			want: `no "other"`,
		},
		{
			name: "plural forms mixed with a namespace",
			tree: map[string]any{"a": map[string]any{"one": "bir", "baslik": "x"}},
			want: "either plural forms or a namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]entry{}
			err := flatten("test.toml", "", tc.tree, out)
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
	cat := &Language{entries: map[string]entry{"x": {text: "%d dakika önce"}}}
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
	base := testCatalogs(t).Base()
	_, trees, err := parsePages(fixtureFuncs(base))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTemplateKeys(trees, base); err != nil {
		t.Fatal(err)
	}
}

// TestNewRefusesATemplateKeyThatDoesNotExist proves the startup check
// actually stops the binary, rather than being a function nobody calls.
func TestNewRefusesATemplateKeyThatDoesNotExist(t *testing.T) {
	cats := testCatalogs(t)
	// The base pack missing one key the layout names.
	delete(cats.Base().entries, "gezinme.atla")
	assets, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cats, assets, nil); err == nil {
		t.Fatal("New accepted a base pack missing a key the templates use")
	} else if !strings.Contains(err.Error(), "gezinme.atla") {
		t.Fatalf("error %q does not name the missing key", err)
	}
}

// TestNewStartsWithAnIncompleteTranslation is the other half of that
// rule, and the deliberate asymmetry. One missing key in a translation
// must not take down a deployment whose readers do not speak that
// language; the page falls back and the gap is reported.
func TestNewStartsWithAnIncompleteTranslation(t *testing.T) {
	cats := testCatalogs(t)
	var secondary *Language
	for _, lang := range cats.Languages() {
		if lang != cats.Base() {
			secondary = lang
			break
		}
	}
	if secondary == nil {
		t.Skip("only the base language is present")
	}
	delete(secondary.entries, "gezinme.atla")
	cats.gaps[secondary.Code] = missingKeys(cats.Base(), secondary)

	assets, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	if _, err := New(cats, assets, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
		t.Fatalf("New refused to start over one untranslated key: %v", err)
	}
	if !strings.Contains(logged.String(), "gezinme.atla") {
		t.Errorf("the missing key was not reported at startup: %s", logged.String())
	}
	// And the page still reads, in the base language.
	if got := secondary.T("gezinme.atla"); got != cats.Base().T("gezinme.atla") {
		t.Errorf("the fallback returned %q", got)
	}
}

// TestEveryErrorStatusHasItsOwnWording covers the keys that are built
// at runtime and are therefore invisible to the template walk.
func TestEveryErrorStatusHasItsOwnWording(t *testing.T) {
	cats := testCatalogs(t)
	for _, lang := range cats.Languages() {
		for _, status := range mappedErrorStatuses {
			titleKey, bodyKey := errorKeys(status)
			if !lang.Has(titleKey) || !lang.Has(bodyKey) {
				t.Errorf("%s: status %d maps to %s/%s which the pack does not define", lang.Code, status, titleKey, bodyKey)
			}
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
	cat := testCatalogs(t).Base()
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
	for _, key := range computedKeys() {
		used[key] = true
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

// wizardStepIDs and checkStatuses mirror lists that live in other
// packages: the wizard's step order (internal/panel/web) and the
// preflight check statuses (internal/panel). They are duplicated here
// rather than imported because this package deliberately knows nothing
// about the domain - it renders, it does not decide.
//
// The duplication is the same deal as mappedErrorStatuses above: adding
// an entry in one place and not the other surfaces, from one side as a
// missing translation and from the other as a key nothing uses.
var (
	wizardStepIDs = []string{"baslangic", "veritabani", "siteler", "toplama", "saklama", "kontrol", "devir"}
	// welcomeStepIDs mirrors welcomeSteps in internal/panel/web. Same
	// deal as the list above: the titles are assembled at runtime from
	// the step id, so neither the template walk nor the source scan sees
	// the family.
	welcomeStepIDs = []string{"site", "saat", "olcum", "ekip"}
	checkStatuses  = []string{"pass", "fail", "warn", "skip"}
	// memberRoles mirrors panel.ValidRoles. The role labels are looked
	// up as "rol." + the stored role, from both a handler and a
	// template, so neither the source scan nor the template walk sees
	// the whole family.
	memberRoles = []string{"owner", "admin", "viewer"}
)

// computedKeys lists the keys assembled at runtime, which the template
// walk cannot see.
func computedKeys() []string {
	keys := []string{}
	for _, id := range wizardStepIDs {
		keys = append(keys, "kurulum.adim."+id+".baslik")
	}
	for _, status := range checkStatuses {
		keys = append(keys, "kontrol.durum."+status)
	}
	for _, role := range memberRoles {
		keys = append(keys, "rol."+role)
	}
	for _, id := range welcomeStepIDs {
		keys = append(keys, "hosgeldiniz.adim."+id+".baslik")
	}
	return keys
}

// TestEveryComputedKeyExists covers the families the template walk
// cannot check, in every language. A wizard step whose title is missing
// renders the marker as its <h1>, which is the most visible possible
// place to discover it.
func TestEveryComputedKeyExists(t *testing.T) {
	cats := testCatalogs(t)
	for _, lang := range cats.Languages() {
		for _, key := range computedKeys() {
			if !lang.Has(key) {
				t.Errorf("%s does not define %q", lang.Code, key)
			}
		}
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
func fixtureFuncs(cat *Language) map[string]any {
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
