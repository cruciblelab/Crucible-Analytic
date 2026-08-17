package ui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The tests in this file check the source of the templates and the
// stylesheet rather than the output of any handler.
//
// They exist because the Content-Security-Policy in headers.go allows
// neither 'unsafe-inline' nor 'unsafe-eval', and every one of the
// patterns below would need one of the two. Without these tests the
// sequence is predictable: somebody adds one inline onclick, the page
// silently does nothing in the browser, and the fix that looks obvious
// is to loosen the policy for the whole panel. Failing here instead
// makes the cheap fix the correct one.

func readUISources(t *testing.T, exts ...string) map[string]string {
	t.Helper()
	want := map[string]bool{}
	for _, e := range exts {
		want[e] = true
	}
	out := map[string]string{}
	for _, root := range []struct {
		fsys fs.FS
		dir  string
	}{{templateFS, "templates"}, {staticFS, "static"}} {
		err := fs.WalkDir(root.fsys, root.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !want[path.Ext(p)] {
				return err
			}
			body, err := fs.ReadFile(root.fsys, p)
			if err != nil {
				return err
			}
			out[p] = stripTemplateComments(string(body))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no sources with extensions %v; the walk is not finding anything", exts)
	}
	return out
}

// templateComment matches {{/* … */}} in all its trim-marker spellings.
var templateComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)

// stripTemplateComments removes what never reaches a browser.
//
// Without this the checks below flag their own explanation: the comment
// in layout.html that says why htmx's inline <style> is turned off
// contains the string "<style", and a test that fails on the note
// describing the fix teaches everyone to weaken the test.
func stripTemplateComments(body string) string {
	return templateComment.ReplaceAllString(body, "")
}

func TestNoInlineScriptOrStyle(t *testing.T) {
	sources := readUISources(t, ".html")
	patterns := []struct {
		re  *regexp.Regexp
		why string
	}{
		// A <script> with a body, as opposed to one that only points at
		// a file: anything but whitespace between the tags.
		{regexp.MustCompile(`(?is)<script[^>]*>\s*[^\s<]`), "an inline <script> body needs 'unsafe-inline'"},
		{regexp.MustCompile(`(?i)<style`), "an inline <style> needs 'unsafe-inline' in style-src"},
		{regexp.MustCompile(`(?i)\sstyle\s*=\s*"`), "a style attribute needs 'unsafe-inline' in style-src"},
		{regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*"`), "an on… handler attribute needs 'unsafe-inline'"},
		{regexp.MustCompile(`(?i)hx-on`), "htmx evaluates hx-on with new Function, which needs 'unsafe-eval'"},
		{regexp.MustCompile(`(?i)hx-vals\s*=\s*"\s*js:`), "js:-prefixed hx-vals is evaluated, which needs 'unsafe-eval'"},
		{regexp.MustCompile(`(?i)javascript:`), "a javascript: URL is script under a different name"},
	}
	for name, body := range sources {
		for _, p := range patterns {
			if loc := p.re.FindString(body); loc != "" {
				t.Errorf("%s contains %q: %s", name, strings.TrimSpace(loc), p.why)
			}
		}
	}
}

// TestNothingIsLoadedFromAnotherOrigin is the deployment promise, not a
// style rule: the panel is one binary, and a page that quietly fetches
// a font or a script from somewhere else would break on the air-gapped
// installations this is meant to run on - and would tell that third
// party who is looking at which customer's panel.
func TestNothingIsLoadedFromAnotherOrigin(t *testing.T) {
	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*"\s*(https?:)?//`)
	for name, body := range readUISources(t, ".html") {
		if loc := external.FindString(body); loc != "" {
			t.Errorf("%s loads from another origin: %q", name, strings.TrimSpace(loc))
		}
	}
	// url() in the stylesheet is the other way a request escapes.
	cssExternal := regexp.MustCompile(`(?i)url\(\s*['"]?\s*(https?:)?//`)
	cssImport := regexp.MustCompile(`(?i)@import`)
	for name, body := range readUISources(t, ".css") {
		if loc := cssExternal.FindString(body); loc != "" {
			t.Errorf("%s fetches from another origin: %q", name, loc)
		}
		if loc := cssImport.FindString(body); loc != "" {
			t.Errorf("%s uses @import: %q", name, loc)
		}
	}
}

// TestAssetsAreReferencedThroughTheHashingHelper catches the other way
// a stylesheet reference goes wrong: hard-coding "/varlik/panel.css"
// works in development, ships a URL with no hash, and then serves a
// year-stale file to everyone after the next edit.
func TestAssetsAreReferencedThroughTheHashingHelper(t *testing.T) {
	literal := regexp.MustCompile(regexp.QuoteMeta(AssetPrefix))
	for name, body := range readUISources(t, ".html") {
		if loc := literal.FindString(body); loc != "" {
			t.Errorf("%s hard-codes %q instead of calling {{asset \"…\"}}", name, loc)
		}
	}
}

// TestThePolicyItselfStaysStrict guards the two directives everything
// above is protecting. A test that only checks the templates would pass
// happily on the day somebody adds 'unsafe-inline' to the policy and
// makes all of it moot.
func TestThePolicyItselfStaysStrict(t *testing.T) {
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(contentSecurityPolicy, forbidden) {
			t.Errorf("the CSP contains %q", forbidden)
		}
	}
	for _, required := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
	} {
		if !strings.Contains(contentSecurityPolicy, required) {
			t.Errorf("the CSP is missing %q", required)
		}
	}
}
