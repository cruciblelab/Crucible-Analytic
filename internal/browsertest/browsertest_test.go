package browsertest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEmpty creates a file whose contents do not matter; only its
// existence does, because resolution checks before it substitutes.
func writeEmpty(path string) error { return os.WriteFile(path, nil, 0o600) }

// sample is the shape every browser script in this repository starts
// with, reduced to the two lines this package rewrites.
const sample = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;
const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
`

// existingFile is a path that certainly exists, for the cases that need
// one: the resolution deliberately checks before it substitutes.
func existingFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "playwright.js")
	if err := writeEmpty(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPrepare_SubstitutesBothPaths.
func TestPrepare_SubstitutesBothPaths(t *testing.T) {
	module := existingFile(t)
	chromium := existingFile(t)
	t.Setenv(EnvModule, module)
	t.Setenv(EnvChromium, chromium)

	out, err := Prepare(sample)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.Contains(out, module) {
		t.Errorf("the module path was not substituted:\n%s", out)
	}
	if !strings.Contains(out, chromium) {
		t.Errorf("the browser path was not substituted:\n%s", out)
	}
	// And the machine-specific literals are gone rather than merely
	// joined by the new ones. A script that still carries the old path
	// runs on one machine and fails on every other.
	if strings.Contains(out, scriptModuleLiteral) {
		t.Error("the development container's module path survived the rewrite")
	}
	if strings.Contains(out, "/opt/pw-browsers/chromium") {
		t.Error("the development container's browser path survived the rewrite")
	}
}

// TestPrepare_WithNoBrowserNamedSaysNothing.
//
// The CI case, and the reason defaultChromium is a var. A runner that
// let Playwright install its own browser has neither the environment
// variable nor the development container's directory, and Playwright
// finds the browser it installed without being told. Passing an
// executablePath that does not exist would fail with a message about a
// missing file, which is a worse error than the one it replaces.
func TestPrepare_WithNoBrowserNamedSaysNothing(t *testing.T) {
	t.Setenv(EnvModule, existingFile(t))
	t.Setenv(EnvChromium, "")

	restore := defaultChromium
	defaultChromium = filepath.Join(t.TempDir(), "not-here")
	t.Cleanup(func() { defaultChromium = restore })

	out, err := Prepare(sample)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.Contains(out, "chromium.launch({})") {
		t.Errorf("launch options are not empty; the script would point at a browser that is not "+
			"there:\n%s", out)
	}
}

// TestPrepare_AnExplicitPathThatIsWrongIsAnError.
//
// Falling through to another candidate would be the friendly thing and
// the wrong one: somebody said where the module is and was mistaken,
// and quietly using a different one hides the mistake until the next
// person wonders why their setting does nothing.
func TestPrepare_AnExplicitPathThatIsWrongIsAnError(t *testing.T) {
	t.Setenv(EnvModule, filepath.Join(t.TempDir(), "not-here.js"))
	if _, err := Prepare(sample); err == nil {
		t.Fatal("a module path that does not exist was accepted")
	} else if !strings.Contains(err.Error(), EnvModule) {
		t.Errorf("the error does not name the setting that is wrong: %v", err)
	}
}

// TestPrepare_RefusesAScriptItCannotRewrite is the whole reason this is
// a function and not a string replace at each call site.
//
// A silent no-op would leave the development container's path in the
// script: the test would pass here and fail everywhere else, and the
// failure would name Playwright rather than this package.
func TestPrepare_RefusesAScriptItCannotRewrite(t *testing.T) {
	t.Setenv(EnvModule, existingFile(t))

	for name, script := range map[string]string{
		"no import": "const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });",
		"no launch": "import playwright from '/opt/node22/lib/node_modules/playwright/index.js';",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Prepare(script); err == nil {
				t.Error("a script with nothing to rewrite was accepted")
			}
		})
	}
}
