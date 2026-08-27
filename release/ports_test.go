// No two shipped defaults claim the same port.
//
// Found by running the whole product from its own tarball: the
// collector's backend_addr - the customer's website - and the read
// API's listen_addr both defaulted to 127.0.0.1:8080. Every service
// started, nothing logged a fault, and an operator who edited neither
// had pointed their traffic proxy at the analytics API. The site would
// have answered every visitor with JSON.
//
// It is the worst kind of misconfiguration: two files, each correct on
// its own, wrong only when read together - which is never, because
// nobody reads four config files side by side.
//
// The example files are what an install copies, so they are the thing
// to check. No build tag: it reads four files.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// addrLine finds a `key = "host:port"` assignment, ignoring comments.
var addrLine = regexp.MustCompile(`(?m)^\s*(listen_addr|backend_addr|analytics_api_url)\s*=\s*"([^"]*)"`)

// port pulls the port off an address or a URL, and returns "" for
// anything without one.
func port(value string) string {
	value = strings.TrimPrefix(strings.TrimPrefix(value, "http://"), "https://")
	value = strings.TrimSuffix(value, "/")
	i := strings.LastIndex(value, ":")
	if i < 0 {
		return ""
	}
	return value[i+1:]
}

// TestNoTwoShippedDefaultsShareAPort.
//
// analytics_api_url is included deliberately: it is not a port anybody
// binds, it is the panel's copy of one, and a copy that drifts from
// what the API listens on is the same class of two-files-disagree fault
// as the collision this test was written for. So it must match the
// API's listen_addr rather than be unique.
func TestNoTwoShippedDefaultsShareAPort(t *testing.T) {
	root := repoRootFromWD(t)

	type claim struct {
		file  string
		key   string
		value string
	}
	var claims []claim

	for _, name := range []string{
		"config.example.toml",
		"beacon.example.toml",
		"analytics-api.example.toml",
		"panel.example.toml",
	} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range addrLine.FindAllStringSubmatch(string(body), -1) {
			claims = append(claims, claim{file: name, key: m[1], value: m[2]})
		}
	}
	if len(claims) < 6 {
		t.Fatalf("found %d addresses across the example configs, which is fewer than the four services have - is the pattern right?", len(claims))
	}

	// The panel's pointer at the API has to agree with where the API
	// listens. Checked before the uniqueness pass, which would otherwise
	// report the agreement as a collision.
	var apiListen, panelPointsAt string
	for _, c := range claims {
		switch {
		case c.file == "analytics-api.example.toml" && c.key == "listen_addr":
			apiListen = port(c.value)
		case c.key == "analytics_api_url":
			panelPointsAt = port(c.value)
		}
	}
	if apiListen == "" || panelPointsAt == "" {
		t.Fatalf("could not find both the API's listen port (%q) and the panel's pointer at it (%q)", apiListen, panelPointsAt)
	}
	if apiListen != panelPointsAt {
		t.Errorf("the panel reads the API on port %s and the API listens on %s; the panel's site pages will say the numbers cannot be read", panelPointsAt, apiListen)
	}

	seen := map[string]claim{}
	for _, c := range claims {
		if c.key == "analytics_api_url" {
			continue // checked above, and it is a pointer rather than a claim
		}
		p := port(c.value)
		if p == "" {
			t.Errorf("%s: %s = %q has no port", c.file, c.key, c.value)
			continue
		}
		if first, taken := seen[p]; taken {
			t.Errorf("port %s is claimed twice: %s in %s and %s in %s - an operator who edits neither has them fighting",
				p, first.key, first.file, c.key, c.file)
			continue
		}
		seen[p] = c
	}
}

// TestTheComposeFilePublishesOnlyWhatTheInternetNeeds.
//
// In a container the loopback bind is gone. `listen_addr =
// "127.0.0.1:8082"` protects the read API on a server; inside a
// container 127.0.0.1 is that container alone, so the panel could not
// reach the API at all - which is why the entrypoint binds these
// services to the container's own address instead.
//
// What replaces that control is this file: only the collector and the
// beacon are published, because only they are meant to be reached from
// outside. It is one `ports:` line away from being untrue, and the
// failure is a login form and a bearer-token API on the public
// internet - so it is asserted rather than left to review.
//
// Read from the file rather than probed from a running stack. Probing
// proves that nothing happened to be listening; reading proves what the
// file says, which is the thing a customer actually deploys.
func TestTheComposeFilePublishesOnlyWhatTheInternetNeeds(t *testing.T) {
	root := repoRootFromWD(t)
	body := readFile(t, root, "docker/compose.yml")

	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("docker/compose.yml does not parse: %v", err)
	}
	if len(doc.Services) < 4 {
		t.Fatalf("found %d services in docker/compose.yml, which is fewer than this product has", len(doc.Services))
	}

	// Named, not counted. A count passes while somebody swaps which
	// service is exposed.
	mayPublish := map[string]string{
		"collector": "the TLS proxy the internet connects to",
		"beacon":    "the endpoint the page snippet posts to from a visitor's browser",
	}
	for name, svc := range doc.Services {
		if len(svc.Ports) == 0 {
			continue
		}
		if why, ok := mayPublish[name]; !ok {
			t.Errorf("docker/compose.yml publishes %s (%v), which is not meant to be reachable from outside; "+
				"put a TLS-terminating reverse proxy in front of it instead", name, svc.Ports)
		} else {
			t.Logf("%s publishes %v - %s", name, svc.Ports, why)
		}
	}
	for name := range mayPublish {
		if len(doc.Services[name].Ports) == 0 {
			t.Errorf("docker/compose.yml no longer publishes %s; nothing outside can reach it", name)
		}
	}
}
