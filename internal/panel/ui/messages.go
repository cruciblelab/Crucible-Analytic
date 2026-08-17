// Package ui renders the panel's HTML: the language packs, the
// templates, the stylesheet and the one JavaScript library, all
// compiled into the binary.
//
// Nothing here reaches the network at runtime and nothing here needs a
// build step. Deploying the panel is copying one file, which is the
// whole reason the stack is html/template plus htmx rather than
// anything that would arrive over npm.
//
// The package renders; it does not decide. It knows nothing about
// sessions, roles or settings - handlers hand it a page value and it
// turns that into bytes. That boundary is what lets the rendering layer
// be tested without a database.
//
// # Adding a language
//
// Drop a `.toml` file in messages/ and rebuild. There is no list in Go
// to update: the loader walks the directory. Read messages/tr.toml for
// what a pack contains.
package ui

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

//go:embed messages
var messagesFS embed.FS

const messagesDir = "messages"

// BaseLanguageCode is the language that owns the key set.
//
// One language has to be the source of truth, and it is the one the
// product is written in rather than the one a developer defaults to. A
// template naming a key the base pack does not define stops the binary;
// the same key missing from a translation is a report and a fallback.
const BaseLanguageCode = "tr"

// MarkMissing wraps a key that no pack defines, so an untranslated
// string is unmistakable on the page rather than an empty space
// somebody has to notice. Guillemets because no copy in this product
// uses them, so the marker can never be mistaken for real text.
const (
	markPrefix = "«"
	markSuffix = "»"
)

// Language is one loaded pack: its identity, its messages and its
// locale's formatting data.
type Language struct {
	// Tag drives number formatting, casing and plural selection. It
	// comes from the file's declared code, so an unparsable code is a
	// startup error rather than a language that silently formats as
	// English.
	Tag  language.Tag
	Code string
	// Name is the endonym - what speakers of this language call it.
	// "Türkçe", not "Turkish": a language picker that names languages in
	// a language you cannot read is useless to the only people who need
	// it.
	Name string
	// Dir is "ltr" or "rtl", written onto <html>.
	Dir string

	entries map[string]entry
	format  formatPack
	// base is the fallback pack, nil for the base language itself.
	base *Language
}

// entry is one message: either plain text or a set of plural forms.
type entry struct {
	text   string
	plural pluralForms
}

// Catalogs is every loaded language, with the machinery to pick one.
type Catalogs struct {
	base    *Language
	all     []*Language
	byCode  map[string]*Language
	tags    []language.Tag
	matcher language.Matcher
	gaps    map[string][]string
}

// LoadCatalogs reads every language pack embedded in the binary.
func LoadCatalogs() (*Catalogs, error) {
	return loadCatalogsFS(messagesFS, messagesDir)
}

// loadCatalogsFS is the real loader, taking the file system so the
// tests can hand it a synthetic set of languages.
//
// That indirection is the point rather than a convenience: "adding a
// language needs no code change" is a claim, and the only way to
// demonstrate it instead of asserting it is to load a language that
// does not exist in this repository.
func loadCatalogsFS(fsys fs.FS, dir string) (*Catalogs, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("ui: read %s: %w", dir, err)
	}

	c := &Catalogs{
		byCode: make(map[string]*Language),
		gaps:   make(map[string][]string),
	}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".toml" {
			continue
		}
		lang, err := loadLanguage(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if _, clash := c.byCode[lang.Code]; clash {
			return nil, fmt.Errorf("ui: two language packs both declare code %q", lang.Code)
		}
		// The file name and the declared code must agree. They are two
		// independent statements of the same fact, and a deployment
		// hunting for "why is en.toml not loading" would otherwise have
		// nothing to look at.
		if name := strings.TrimSuffix(e.Name(), ".toml"); name != lang.Code {
			return nil, fmt.Errorf("ui: %s declares code %q; the file name and the code must match", e.Name(), lang.Code)
		}
		c.byCode[lang.Code] = lang
		c.all = append(c.all, lang)
	}
	if len(c.all) == 0 {
		return nil, fmt.Errorf("ui: no language packs found in %s", dir)
	}

	base, ok := c.byCode[BaseLanguageCode]
	if !ok {
		return nil, fmt.Errorf("ui: the base language %q is missing from %s", BaseLanguageCode, dir)
	}
	c.base = base

	// Base first: language.Matcher treats the first tag as the default,
	// which is what an empty or unservable Accept-Language should get.
	sort.Slice(c.all, func(i, j int) bool {
		if (c.all[i] == base) != (c.all[j] == base) {
			return c.all[i] == base
		}
		return c.all[i].Code < c.all[j].Code
	})
	for _, lang := range c.all {
		if lang != base {
			lang.base = base
			c.gaps[lang.Code] = missingKeys(base, lang)
		}
		c.tags = append(c.tags, lang.Tag)
	}
	c.matcher = language.NewMatcher(c.tags)
	return c, nil
}

func loadLanguage(fsys fs.FS, filePath string) (*Language, error) {
	raw, err := fs.ReadFile(fsys, filePath)
	if err != nil {
		return nil, fmt.Errorf("ui: read %s: %w", filePath, err)
	}
	var file struct {
		Dil struct {
			Kod string `toml:"kod"`
			Ad  string `toml:"ad"`
			Yon string `toml:"yon"`
		} `toml:"dil"`
		Bicim formatPack     `toml:"bicim"`
		Metin map[string]any `toml:"metin"`
	}
	if _, err := toml.Decode(string(raw), &file); err != nil {
		return nil, fmt.Errorf("ui: parse %s: %w", filePath, err)
	}

	if file.Dil.Kod == "" {
		return nil, fmt.Errorf("ui: %s has no [dil] kod", filePath)
	}
	tag, err := language.Parse(file.Dil.Kod)
	if err != nil {
		return nil, fmt.Errorf("ui: %s declares code %q which is not a language tag: %w", filePath, file.Dil.Kod, err)
	}
	if strings.TrimSpace(file.Dil.Ad) == "" {
		return nil, fmt.Errorf("ui: %s has no [dil] ad (the language's name in its own language)", filePath)
	}
	switch file.Dil.Yon {
	case "ltr", "rtl":
	default:
		return nil, fmt.Errorf("ui: %s has [dil] yon = %q (want \"ltr\" or \"rtl\")", filePath, file.Dil.Yon)
	}
	if err := file.Bicim.validate(filePath); err != nil {
		return nil, err
	}

	lang := &Language{
		Tag:     tag,
		Code:    file.Dil.Kod,
		Name:    file.Dil.Ad,
		Dir:     file.Dil.Yon,
		entries: make(map[string]entry),
		format:  file.Bicim,
	}
	if err := flatten(filePath, "", file.Metin, lang.entries); err != nil {
		return nil, err
	}
	if len(lang.entries) == 0 {
		return nil, fmt.Errorf("ui: %s has no [metin] section", filePath)
	}
	return lang, nil
}

// pluralFormNames are the CLDR categories a message may carry. Closed:
// a table using any of these names is a plural entry, and one using
// none of them is a namespace.
var pluralFormNames = map[string]bool{
	"zero": true, "one": true, "two": true, "few": true, "many": true, "other": true,
}

func flatten(file, prefix string, tree map[string]any, out map[string]entry) error {
	for name, value := range tree {
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("ui: %s: key %q is empty; an empty translation is invisible on the page", file, key)
			}
			if _, clash := out[key]; clash {
				return fmt.Errorf("ui: %s: key %q defined twice", file, key)
			}
			out[key] = entry{text: v}
		case map[string]any:
			forms, isPlural, err := readPluralForms(file, key, v)
			if err != nil {
				return err
			}
			if isPlural {
				out[key] = entry{plural: forms}
				continue
			}
			if err := flatten(file, key, v, out); err != nil {
				return err
			}
		default:
			return fmt.Errorf("ui: %s: key %q is %T; a message is text or a table of plural forms", file, key, value)
		}
	}
	return nil
}

// readPluralForms decides whether a table is a plural entry or a
// namespace, and refuses the shape that is neither.
//
// A table carrying some plural names but no "other" is the interesting
// case: it is always a typo, never a namespace somebody meant, and
// treating it as a namespace would produce a key nobody can reach.
func readPluralForms(file, key string, table map[string]any) (pluralForms, bool, error) {
	var named int
	for name := range table {
		if pluralFormNames[name] {
			named++
		}
	}
	if named == 0 {
		return pluralForms{}, false, nil
	}
	if named != len(table) {
		return pluralForms{}, false, fmt.Errorf(
			"ui: %s: key %q mixes plural forms with other names; a table is either plural forms or a namespace", file, key)
	}
	forms := pluralForms{}
	for name, value := range table {
		text, ok := value.(string)
		if !ok {
			return pluralForms{}, false, fmt.Errorf("ui: %s: key %q form %q is %T, want text", file, key, name, value)
		}
		if strings.TrimSpace(text) == "" {
			return pluralForms{}, false, fmt.Errorf("ui: %s: key %q form %q is empty", file, key, name)
		}
		forms.set(name, text)
	}
	if forms.Other == "" {
		return pluralForms{}, false, fmt.Errorf(
			"ui: %s: key %q has plural forms but no \"other\"; every language falls back to it", file, key)
	}
	return forms, true, nil
}

// pluralForms holds the CLDR categories a message supplies. A language
// fills in only the ones it needs.
type pluralForms struct {
	Zero  string `toml:"zero"`
	One   string `toml:"one"`
	Two   string `toml:"two"`
	Few   string `toml:"few"`
	Many  string `toml:"many"`
	Other string `toml:"other"`
}

func (p *pluralForms) set(name, text string) {
	switch name {
	case "zero":
		p.Zero = text
	case "one":
		p.One = text
	case "two":
		p.Two = text
	case "few":
		p.Few = text
	case "many":
		p.Many = text
	case "other":
		p.Other = text
	}
}

// pick chooses a form for n using real CLDR rules, falling back to
// "other" for any category this language did not supply.
//
// The fallback is what keeps the mechanism invisible to languages that
// do not need it: Turkish supplies "other" alone and never has to know
// plural categories exist, while Russian can supply four and get them.
func (p pluralForms) pick(tag language.Tag, n int) string {
	switch plural.Cardinal.MatchPlural(tag, n, 0, 0, 0, 0) {
	case plural.Zero:
		if p.Zero != "" {
			return p.Zero
		}
	case plural.One:
		if p.One != "" {
			return p.One
		}
	case plural.Two:
		if p.Two != "" {
			return p.Two
		}
	case plural.Few:
		if p.Few != "" {
			return p.Few
		}
	case plural.Many:
		if p.Many != "" {
			return p.Many
		}
	}
	return p.Other
}

func (p pluralForms) empty() bool { return p.Other == "" }

// missingKeys lists what base defines and lang does not.
func missingKeys(base, lang *Language) []string {
	var missing []string
	for key := range base.entries {
		if _, ok := lang.entries[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// Has reports whether this language defines the key itself, without
// consulting the fallback.
func (l *Language) Has(key string) bool {
	_, ok := l.entries[key]
	return ok
}

// lookup resolves a key through this language and then the base one.
func (l *Language) lookup(key string) (entry, bool) {
	if e, ok := l.entries[key]; ok {
		return e, true
	}
	if l.base != nil {
		if e, ok := l.base.entries[key]; ok {
			return e, true
		}
	}
	return entry{}, false
}

// T returns the text for key.
//
// A key no pack defines returns the marker rather than "" and rather
// than a panic. The startup check has already refused to run a binary
// whose templates name a key the base language lacks, so reaching this
// means the key was computed at runtime - and there a visibly wrong
// page beats both a blank one and a failed request.
func (l *Language) T(key string) string {
	e, ok := l.lookup(key)
	if !ok {
		return markPrefix + key + markSuffix
	}
	if e.text != "" {
		return e.text
	}
	// A plural message asked for without a count: "other" is the only
	// honest answer, and it is what most languages use for everything
	// anyway.
	return e.plural.Other
}

// Tf is T with fmt verbs filled in.
func (l *Language) Tf(key string, args ...any) string {
	e, ok := l.lookup(key)
	if !ok {
		return markPrefix + key + markSuffix
	}
	text := e.text
	if text == "" {
		text = e.plural.Other
	}
	return fmt.Sprintf(text, args...)
}

// Tn returns the form of key that fits n, with %s replaced by the
// number formatted for this language.
func (l *Language) Tn(key string, n int, formatted string) string {
	e, ok := l.lookup(key)
	if !ok {
		return markPrefix + key + markSuffix
	}
	if e.plural.empty() {
		// Not a plural message. Fill it anyway rather than dropping the
		// count on the floor.
		return fmt.Sprintf(e.text, formatted)
	}
	return fmt.Sprintf(e.plural.pick(l.Tag, n), formatted)
}

// Keys returns every key this language defines, sorted.
func (l *Language) Keys() []string {
	keys := make([]string, 0, len(l.entries))
	for key := range l.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Len reports how many entries this language defines.
func (l *Language) Len() int { return len(l.entries) }

// Base is the language that owns the key set.
func (c *Catalogs) Base() *Language { return c.base }

// Languages returns every loaded pack, base first.
func (c *Catalogs) Languages() []*Language { return c.all }

// ByCode returns one language, or nil.
func (c *Catalogs) ByCode(code string) *Language { return c.byCode[code] }

// Gaps maps a language code to the keys it does not define. Reported
// once at startup; the pages themselves fall back silently, because a
// customer should not lose a panel over somebody else's untranslated
// sentence.
func (c *Catalogs) Gaps() map[string][]string { return c.gaps }

// Match picks a language.
//
// preferred comes first and is for a choice somebody actually made - a
// per-account setting, or the deployment's configured default. The
// Accept-Language header is consulted after that, which is what serves
// a colleague whose browser is set to another language on a deployment
// configured for Turkish.
//
// Anything unservable lands on the base language, because the matcher
// is built with it first.
func (c *Catalogs) Match(acceptLanguage string, preferred ...string) *Language {
	for _, code := range preferred {
		if code == "" {
			continue
		}
		if lang := c.byCode[code]; lang != nil {
			return lang
		}
	}
	if acceptLanguage == "" {
		return c.base
	}
	_, index := language.MatchStrings(c.matcher, acceptLanguage)
	if index < 0 || index >= len(c.all) {
		return c.base
	}
	return c.all[index]
}
