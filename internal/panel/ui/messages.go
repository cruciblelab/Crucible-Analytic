// Package ui renders the panel's HTML: the message catalogue, the
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
package ui

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed messages.tr.toml
var catalogFS embed.FS

// MarkMissing wraps a key that has no entry, so an untranslated string
// is unmistakable on the page rather than an empty space somebody has
// to notice. Guillemets because no Turkish copy in this product uses
// them, so the marker can never be mistaken for real text.
const (
	markPrefix = "«"
	markSuffix = "»"
)

// Catalog is the panel's Turkish text, keyed by dotted path.
//
// One language, deliberately. The product is sold in Turkish and every
// string in it was written to be read by a shop owner, not translated
// from English defaults. A second language would mean a second catalog
// file and a language on the request; nothing else in this package
// assumes there is only one.
//
// Turkish also spares this package the machinery an English-first
// design would have grown by reflex: a count does not inflect the noun
// after it ("1 ziyaretçi", "3 ziyaretçi"), so there is no plural rule
// engine here and there does not need to be one.
type Catalog struct {
	entries map[string]string
}

// LoadCatalog parses the embedded catalog.
//
// It fails rather than tolerating: a non-string value, an empty string,
// or a key that collides with a table are all errors. An empty
// translation is the worst of the three, because on the page it is
// indistinguishable from a key nobody ever wrote.
func LoadCatalog() (*Catalog, error) {
	raw, err := catalogFS.ReadFile("messages.tr.toml")
	if err != nil {
		return nil, fmt.Errorf("ui: read catalog: %w", err)
	}
	var tree map[string]any
	if err := toml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("ui: parse catalog: %w", err)
	}
	entries := make(map[string]string)
	if err := flatten("", tree, entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("ui: catalog is empty")
	}
	return &Catalog{entries: entries}, nil
}

func flatten(prefix string, tree map[string]any, out map[string]string) error {
	for name, value := range tree {
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("ui: catalog key %q is empty; an empty translation is invisible on the page", key)
			}
			if _, clash := out[key]; clash {
				return fmt.Errorf("ui: catalog key %q defined twice", key)
			}
			out[key] = v
		case map[string]any:
			if err := flatten(key, v, out); err != nil {
				return err
			}
		default:
			return fmt.Errorf("ui: catalog key %q is %T; only text is allowed", key, value)
		}
	}
	return nil
}

// Has reports whether the key exists. Used by the startup check that
// walks the templates, and by tests.
func (c *Catalog) Has(key string) bool {
	_, ok := c.entries[key]
	return ok
}

// T returns the text for key.
//
// A missing key returns the marker rather than "" and rather than a
// panic. The startup check has already refused to run a binary whose
// templates name a key that does not exist, so reaching this branch
// means the key was computed at runtime - and in that case a visibly
// wrong page is better than both a blank one and a crashed request.
func (c *Catalog) T(key string) string {
	if text, ok := c.entries[key]; ok {
		return text
	}
	return markPrefix + key + markSuffix
}

// Tf is T with fmt verbs filled in.
func (c *Catalog) Tf(key string, args ...any) string {
	text, ok := c.entries[key]
	if !ok {
		return markPrefix + key + markSuffix
	}
	return fmt.Sprintf(text, args...)
}

// Keys returns every key, sorted. For tests and for the completeness
// check that runs the other direction.
func (c *Catalog) Keys() []string {
	keys := make([]string, 0, len(c.entries))
	for key := range c.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Len reports how many entries the catalog holds.
func (c *Catalog) Len() int { return len(c.entries) }
