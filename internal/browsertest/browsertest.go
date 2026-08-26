// Package browsertest resolves the two machine-specific paths the
// browser tests need, so that the same test runs on a developer's
// machine and on a CI runner without either one being special-cased.
//
// # Why this exists
//
// The browser tests drive a real Chromium through Playwright by writing
// a short ESM script to a temporary directory and running it with node.
// A script in a temp directory cannot resolve `playwright` by name -
// there is no node_modules above it - so it has to import the module by
// absolute path, and that path was written into nine scripts across
// eight files as the literal this project's development container
// happens to use:
//
//	/opt/node22/lib/node_modules/playwright/index.js
//
// That is a fact about one machine. On a GitHub runner the module lives
// somewhere else and Chromium lives somewhere else again, so the whole
// browser suite - which G1 makes a merge gate - could not have run
// there at all.
//
// # Asking rather than guessing
//
// The module path is not guessed from a list of likely locations. node
// already knows where its global modules are and will say so (`npm root
// -g`), so the resolution order is: what the environment was told, then
// what node says, then the historical default. Each candidate is checked
// for existence before it is used.
//
// Chromium is the same question with a different answer at the end: if
// nothing names one, the path is removed from the script entirely rather
// than defaulted, because a Playwright that installed its own browser
// finds it without help - and pointing it at a binary that is not there
// fails worse than saying nothing.
package browsertest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The literals the scripts are written with. They are also the last
// candidate tried, so a machine set up like the development container
// keeps working with nothing configured.
const (
	scriptModuleLiteral   = "/opt/node22/lib/node_modules/playwright/index.js"
	scriptChromiumLiteral = "{ executablePath: '/opt/pw-browsers/chromium' }"
)

// defaultChromium is a var rather than a const so the package's own test
// can exercise the branch this machine cannot reach: what happens when
// no browser is named anywhere. That is not a hypothetical - it is what
// every CI runner looks like - and a branch only CI can reach is a
// branch nobody can debug when it breaks.
var defaultChromium = "/opt/pw-browsers/chromium"

// Environment overrides, for a machine that has these somewhere else and
// for CI, which does.
const (
	EnvModule   = "CA_PLAYWRIGHT_JS"
	EnvChromium = "CA_CHROMIUM"
)

// Prepare returns script with the machine-specific paths replaced by
// ones that exist here.
//
// It fails rather than returning the script unchanged when a literal it
// expected is missing. That matters more than it looks: a silent no-op
// would leave the original hard-coded path in the script, the test would
// run on the development container and fail everywhere else, and the
// failure would point at Playwright rather than at this function.
func Prepare(script string) (string, error) {
	module, err := modulePath()
	if err != nil {
		return "", err
	}
	if !strings.Contains(script, scriptModuleLiteral) {
		return "", fmt.Errorf("browsertest: the script does not import %q, so there is nothing to "+
			"rewrite - if the import moved, this package has to move with it", scriptModuleLiteral)
	}
	script = strings.ReplaceAll(script, scriptModuleLiteral, module)

	if !strings.Contains(script, scriptChromiumLiteral) {
		return "", fmt.Errorf("browsertest: the script does not launch with %q, so the browser path "+
			"cannot be corrected for this machine", scriptChromiumLiteral)
	}
	return strings.ReplaceAll(script, scriptChromiumLiteral, launchOptions()), nil
}

// modulePath is where playwright's ESM entry point actually is.
func modulePath() (string, error) {
	tried := []string{}

	if p := os.Getenv(EnvModule); p != "" {
		if exists(p) {
			return p, nil
		}
		// An explicit setting that is wrong is an error, not something
		// to fall through: somebody said where it is and was mistaken,
		// and silently using a different one hides that.
		return "", fmt.Errorf("browsertest: %s is set to %q, which does not exist", EnvModule, p)
	}

	// node knows where its own global modules are; asking beats keeping
	// a list of the places they tend to be.
	if root, err := exec.Command("npm", "root", "-g").Output(); err == nil {
		p := filepath.Join(strings.TrimSpace(string(root)), "playwright", "index.js")
		if exists(p) {
			return p, nil
		}
		tried = append(tried, p)
	}

	if exists(scriptModuleLiteral) {
		return scriptModuleLiteral, nil
	}
	tried = append(tried, scriptModuleLiteral)

	return "", fmt.Errorf("browsertest: no playwright module found (tried %s); "+
		"install it (npm install -g playwright) or set %s",
		strings.Join(tried, ", "), EnvModule)
}

// launchOptions is the object literal the script passes to
// chromium.launch.
func launchOptions() string {
	if p := os.Getenv(EnvChromium); p != "" {
		return fmt.Sprintf("{ executablePath: '%s' }", p)
	}
	if exists(defaultChromium) {
		return fmt.Sprintf("{ executablePath: '%s' }", defaultChromium)
	}
	// Nothing named one, so say nothing. Playwright installs a browser
	// of its own and knows where it put it; a wrong executablePath would
	// fail with a message about a missing file rather than about a
	// missing browser.
	return "{}"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
