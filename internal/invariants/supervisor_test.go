package invariants

import (
	"bufio"
	"bytes"
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
)

// PLAN §4 #38 (B3e): the half of the restart that is not ours.
//
// A restart request makes a service exit cleanly - exit status 0 - and
// nothing in this product starts it again. Its supervisor does: systemd
// with Restart=always, Docker with restart: unless-stopped. Those two
// lines are the whole second half of the mechanism, and they live in
// files no Go test read. Restart=on-failure, the most common value in
// unit files anywhere, does not restart a process that exited 0 - so a
// unit edited to it would turn the panel's restart button into a stop
// button, with nothing failing anywhere.
//
// So, for every service whose main writes a heartbeat (the services the
// panel offers to restart; the list is the tree's):
//
//   - its systemd unit says Restart=always, in [Service];
//   - its compose service says restart: unless-stopped;
//   - the unit's start limit cannot be reached by the panel's button
//     alone: within StartLimitIntervalSec, requests spaced RestartSpacing
//     apart start the unit at most interval/spacing + 1 times, and that
//     must stay below StartLimitBurst, or a person pressing the button
//     leaves the unit in the failed state systemd gives up on.

func TestEveryRestartableServiceIsRestartedByItsSupervisor(t *testing.T) {
	root := repoRoot(t)
	beating := heartbeatMains(t, root)
	if len(beating) < 4 {
		t.Fatalf("found %d mains calling heartbeat.New (%v), want at least 4 - the rule "+
			"below would check nothing", len(beating), sortedKeys(beating))
	}

	// systemd.
	units, err := filepath.Glob(filepath.Join(root, "release", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	inUnit := map[string]bool{}
	for _, path := range units {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := execStart.FindSubmatch(body)
		if m == nil || !beating[string(m[1])] {
			continue
		}
		name, unit := string(m[1]), filepath.Base(path)
		inUnit[name] = true
		sections := unitSections(body)

		if got := sections["Service"]["Restart"]; got != "always" {
			t.Errorf("%s runs %s, which the panel can ask to restart, and says Restart=%q "+
				"in [Service], want \"always\".\nA restart request ends the process with "+
				"status 0; anything but always (on-failure, the usual value) leaves it stopped",
				unit, name, got)
		}

		burst, interval, ok := startLimit(t, unit, sections["Unit"])
		if !ok {
			continue
		}
		starts := int(interval/panel.RestartSpacing) + 1
		if starts >= burst {
			t.Errorf("%s: StartLimitBurst=%d in %s, and requests the panel spaces %s apart "+
				"start the unit up to %d times in that window.\nPressing the button would "+
				"leave the unit failed; the burst must stay above %d",
				unit, burst, interval, panel.RestartSpacing, starts, starts)
		}
	}

	// Docker.
	raw, err := os.ReadFile(filepath.Join(root, "docker", "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Command any    `yaml:"command"`
			Restart string `yaml:"restart"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("docker/compose.yml: %v", err)
	}
	inCompose := map[string]bool{}
	for service, def := range compose.Services {
		name := firstWord(def.Command)
		if !beating[name] {
			continue
		}
		inCompose[name] = true
		if def.Restart != "unless-stopped" {
			t.Errorf("docker/compose.yml: service %s runs %s, which the panel can ask to "+
				"restart, and says restart: %q, want \"unless-stopped\".\nA restart request "+
				"ends the process with status 0; \"no\" and \"on-failure\" leave it stopped",
				service, name, def.Restart)
		}
	}

	// Each service is under both supervisors: a binary missing from one
	// of them is a binary whose restart nobody checked here.
	for _, name := range sortedKeys(beating) {
		if !inUnit[name] {
			t.Errorf("cmd/%s writes a heartbeat and no unit in release/systemd runs it, "+
				"so whether systemd starts it again after a restart request is unchecked", name)
		}
		if !inCompose[name] {
			t.Errorf("cmd/%s writes a heartbeat and no service in docker/compose.yml runs it, "+
				"so whether Docker starts it again after a restart request is unchecked", name)
		}
	}
}

// The panel's warning beside the collector's restart button states three
// numbers, measured on the real binaries under a Restart=always
// supervisor with RestartSec=2 (B3e): new connections were refused for
// 2.04 s (three restarts, 2.035-2.046 s) with nothing in flight, and for
// 12.04 s while one download was running - the drain waited its 10 s,
// cut the download, and the supervisor's 2 s followed. Two of those
// numbers belong to settings, so the sentence is tied to them here: the
// unit's RestartSec, proxy.DrainTimeout, and their sum. A sentence that
// kept saying 2 and 12 after either moved would be the panel promising
// a customer an outage it no longer has, or hiding one it does.
var collectorWarning = regexp.MustCompile(`(?m)^uyari_collector = "(.*)"$`)

func TestTheCollectorRestartWarningStatesTheMeasuredNumbers(t *testing.T) {
	root := repoRoot(t)
	unit, err := os.ReadFile(filepath.Join(root, "release", "systemd", "crucible-collector.service"))
	if err != nil {
		t.Fatal(err)
	}
	restartSec := unitSections(unit)["Service"]["RestartSec"]
	secs, err := strconv.Atoi(strings.TrimSuffix(restartSec, "s"))
	if err != nil {
		t.Fatalf("crucible-collector.service: RestartSec=%q is not a number of seconds this test reads", restartSec)
	}
	drain := int(proxy.DrainTimeout / time.Second)
	want := []int{secs, drain, secs + drain}

	for _, lang := range []string{"tr", "en"} {
		body, err := os.ReadFile(filepath.Join(root, "internal", "panel", "ui", "messages", lang+".toml"))
		if err != nil {
			t.Fatal(err)
		}
		m := collectorWarning.FindSubmatch(body)
		if m == nil {
			t.Errorf("messages/%s.toml has no uyari_collector line; the warning moved", lang)
			continue
		}
		sentence := string(m[1])
		for _, n := range want {
			if !regexp.MustCompile(`(^|[^0-9])` + strconv.Itoa(n) + `([^0-9]|$)`).MatchString(sentence) {
				t.Errorf("messages/%s.toml uyari_collector does not say %d.\nThe outage is the "+
					"unit's RestartSec (%ds) with nothing in flight and the drain (%ds) plus that "+
					"with a download running; the sentence is: %s", lang, n, secs, drain, sentence)
			}
		}
	}
}

// heartbeatMains names every cmd/<name> whose main calls heartbeat.New.
func heartbeatMains(t *testing.T, root string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, f := range goFilesIn(t, filepath.Join(root, "cmd", entry.Name())) {
			hb := localName(f, heartbeatPath)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Name.Name == "main" && fn.Recv == nil && fn.Body != nil &&
					len(callsOf(fn.Body, hb, "New")) > 0 {
					out[entry.Name()] = true
				}
			}
		}
	}
	return out
}

var unitSection = regexp.MustCompile(`^\[([A-Za-z]+)\]$`)

// unitSections reads a unit file into section -> key -> value. A key set
// twice keeps the last value, as systemd does for these settings.
func unitSections(body []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if m := unitSection.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if out[section] == nil {
			out[section] = map[string]string{}
		}
		out[section][strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

// startLimit reads StartLimitBurst and StartLimitIntervalSec from a
// unit's [Unit] section - the only section systemd reads them from.
func startLimit(t *testing.T, unit string, section map[string]string) (int, time.Duration, bool) {
	t.Helper()
	b, i := section["StartLimitBurst"], section["StartLimitIntervalSec"]
	if b == "" || i == "" {
		t.Errorf("%s has no StartLimitBurst/StartLimitIntervalSec in [Unit] (burst %q, "+
			"interval %q); the restart button's distance from the start limit is unknown",
			unit, b, i)
		return 0, 0, false
	}
	burst, err := strconv.Atoi(b)
	if err != nil {
		t.Errorf("%s: StartLimitBurst=%q is not a number", unit, b)
		return 0, 0, false
	}
	// Seconds, bare or with an "s": the spelling these units use. Any
	// other spelling is refused rather than guessed at.
	secs, err := strconv.Atoi(strings.TrimSuffix(i, "s"))
	if err != nil {
		t.Errorf("%s: StartLimitIntervalSec=%q is not a number of seconds this test reads", unit, i)
		return 0, 0, false
	}
	return burst, time.Duration(secs) * time.Second, true
}

// firstWord is the program a compose command runs: the first element of
// a list, or the first word of a string.
func firstWord(command any) string {
	switch c := command.(type) {
	case []any:
		if len(c) > 0 {
			if s, ok := c[0].(string); ok {
				return s
			}
		}
	case string:
		if f := strings.Fields(c); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}
