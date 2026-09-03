//go:build release

// The release package, verified by building it.
//
// Its own build tag rather than `integration`: this compiles ten binaries
// and does it twice, which is minutes rather than seconds, and it needs
// no database. Nightly, alongside the load tests, for the same reason
// they are there - a gate that takes minutes is a gate people route
// around.
//
//	go test -tags release ./release/
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the directory above this one.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

// build runs build.sh in dir, writing the package under out, and returns
// the staging directory.
func build(t *testing.T, dir, out, version string) string {
	t.Helper()
	cmd := exec.Command("./release/build.sh")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VERSION="+version, "OUT="+out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build.sh in %s: %v\n%s", dir, err, combined)
	}
	return filepath.Join(out, "crucible-analytic-"+version)
}

// TestThePackageIsReproducible is the phase's headline claim, and the
// reason it is tested from two *directories* rather than twice in one.
//
// The point of a reproducible release is that somebody who downloaded the
// source can rebuild it and get the same bytes. That person has a
// tarball, not a repository - so a build that embeds anything about the
// build machine or its git state is one they can never match, and the
// claim becomes untestable by precisely the person it exists for.
//
// Building twice in one directory would pass while that was broken. It
// did: with VCS embedding left on, this test's two builds produced five
// different checksums each time, and -trimpath was working correctly in
// both. Hence -buildvcs=false in build.sh, and hence the export below.
func TestThePackageIsReproducible(t *testing.T) {
	root := repoRoot(t)
	const version = "v0.0.0-repro"

	// An export of the working tree, with no .git in it: what somebody
	// who downloaded the source actually has.
	elsewhere := t.TempDir()
	export(t, root, elsewhere)

	first := build(t, root, t.TempDir(), version)
	second := build(t, elsewhere, t.TempDir(), version)

	a := readSums(t, first)
	b := readSums(t, second)

	if len(a) == 0 {
		t.Fatal("the first build produced no checksums; this test would pass by comparing nothing")
	}
	for name, sum := range a {
		other, ok := b[name]
		if !ok {
			t.Errorf("%s is in one package and not the other", name)
			continue
		}
		if sum != other {
			t.Errorf("%s differs between two builds of the same source:\n  %s\n  %s", name, sum, other)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			t.Errorf("%s is in the second package and not the first", name)
		}
	}
}

// export copies the working tree into dst without .git, so the build
// there has no repository to read.
func export(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("sh", "-c",
		"tar -C "+src+" --exclude=.git --exclude=dist -cf - . | tar -C "+dst+" -xf -")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exporting the tree: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		t.Fatal("the export still has a .git; it would read the repository and this test would prove nothing")
	}
}

// readSums parses the package's SHA256SUMS into name -> checksum.
func readSums(t *testing.T, stage string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(stage, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			out[fields[1]] = fields[0]
		}
	}
	return out
}

// TestThePackageCarriesWhatAnInstallNeeds.
//
// Named individually rather than counted. A count passes while somebody
// swaps a schema for a second copy of another, and every one of these is
// a file whose absence only shows up part-way through somebody's install.
func TestThePackageCarriesWhatAnInstallNeeds(t *testing.T) {
	stage := build(t, repoRoot(t), t.TempDir(), "v0.0.0-contents")

	for _, want := range []string{
		"bin/collector", "bin/beacon", "bin/analytics-api", "bin/panel", "bin/devpass",
		"bin/upgrader",
		"schema/01-panel.sql", "schema/02-storage.sql",
		"schema/03-beacon.sql", "schema/04-asnlookup.sql",
		"schema/05-heartbeat.sql", "schema/06-retention.sql",
		"schema/07-logsink.sql", "schema/08-upgrade.sql",
		"schema/09-rangerefresh.sql", "schema/10-schemaver.sql",
		"systemd/crucible-collector.service", "systemd/crucible-beacon.service",
		"systemd/crucible-analytics-api.service", "systemd/crucible-panel.service",
		"systemd/crucible-upgrader.service",
		// The timer, and it is the half that matters: the service above
		// has no [Install] section, so a package carrying the service
		// without the timer installs an upgrader that never runs.
		"systemd/crucible-upgrader.timer",
		"ornek-yapilandirma/config.example.toml",
		"ornek-yapilandirma/beacon.example.toml",
		"ornek-yapilandirma/analytics-api.example.toml",
		"ornek-yapilandirma/panel.example.toml",
		"ornek-yapilandirma/upgrader.example.toml",
		"LICENSE", "NOTICE", "THIRD-PARTY.md", "KURULUM.md", "README.md",
		"SHA256SUMS",
		// The installer and its SQL. A package that told the operator to
		// clone a repository to install it would be handing back the
		// manual work F2 exists to remove.
		"release/install.sh", "release/verify.sh",
		"release/sql/grants.sql", "release/sql/verify.sql", "release/sql/harden.sql",
	} {
		if _, err := os.Stat(filepath.Join(stage, want)); err != nil {
			t.Errorf("the package has no %s", want)
		}
	}
}

// TestEveryPackagedBinaryReportsItsVersion.
//
// The measured finding this phase exists for: KURULUM.md told operators
// to stamp all five with -X main.version, and the symbol existed in one.
// The linker does not warn about a symbol that is not there, so four of
// the five iterations of the documented command silently did nothing.
//
// This asserts the fix where it matters - on the binaries that actually
// ship, built by the script that actually builds them.
// notShipped are the commands that must stay out of the package, with
// the reason each one is out.
//
// An exclusion list, which is normally the thing this file argues
// against - but the alternative here is worse. Without it the choice is
// to ship a maintainer's tool to every customer, or to weaken the check
// above into one that no longer notices a command build.sh was never
// told about. This keeps the check total and makes the exception a
// sentence somebody has to write.
//
// It is not a free pass in either direction: a name here that turns up
// in the package fails too, so "excluded" cannot quietly become
// "shipped anyway".
var notShipped = map[string]string{
	"releasesign": "the signing tool. It is how a release is made, not part of one, " +
		"and the capability it represents belongs to whoever holds the key. " +
		"Shipping it would be harmless - it cannot sign without CA_RELEASE_KEY - " +
		"and it would still be the wrong signal in a customer's bin directory",
}

func TestEveryPackagedBinaryReportsItsVersion(t *testing.T) {
	const version = "v0.0.0-stamp"
	stage := build(t, repoRoot(t), t.TempDir(), version)

	// Read from the package rather than listed here, and the difference
	// is not cosmetic. The hand list said five, the sixth binary arrived
	// with L3, and nothing noticed that `upgrader -version` printed a
	// bare version string while the other five named themselves - which
	// is precisely the defect the comment above says this test exists
	// for, reintroduced under a list that had stopped being complete.
	//
	// A one-way list of things to check is a list that stops covering
	// what ships. What ships is in bin/.
	packaged, err := filepath.Glob(filepath.Join(stage, "bin", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(packaged) < 6 {
		t.Fatalf("the package holds %d binaries; this project builds six, so either "+
			"build.sh dropped some or this test is looking in the wrong place",
			len(packaged))
	}

	// And every cmd/ is one of them. A new command that build.sh was
	// never told about is not merely unchecked here, it is absent from
	// the release - and the first person to find out is whoever installs
	// the package expecting it.
	commands, err := filepath.Glob(filepath.Join(repoRoot(t), "cmd", "*"))
	if err != nil {
		t.Fatal(err)
	}
	shipped := map[string]bool{}
	for _, p := range packaged {
		shipped[filepath.Base(p)] = true
	}
	for _, c := range commands {
		name := filepath.Base(c)
		why, excluded := notShipped[name]
		if shipped[name] {
			if excluded {
				t.Errorf("cmd/%s is listed as deliberately unshipped and the package "+
					"contains it. One of the two is wrong, and the exclusion is the "+
					"half somebody wrote down on purpose: %s", name, why)
			}
			continue
		}
		if excluded {
			continue
		}
		t.Errorf("cmd/%s is not in the release package; release/build.sh has a "+
			"hand-written list of binaries and this one is not on it", name)
	}

	for _, path := range packaged {
		name := filepath.Base(path)
		out, err := exec.Command(path, "-version").Output()
		if err != nil {
			t.Errorf("%s -version: %v", name, err)
			continue
		}
		got := strings.TrimSpace(string(out))
		if !strings.Contains(got, version) {
			t.Errorf("%s -version = %q, want it to carry the stamp %q", name, got, version)
		}
		if !strings.Contains(got, name) {
			t.Errorf("%s -version = %q, want it to name itself - support asks which process, "+
				"and six identical lines answer nothing", name, got)
		}
	}
}

// TestVerifyRefusesWhatMustNeverBePackaged.
//
// The scope note promises the "never" list is checked by a machine rather
// than by care. Watching a clean build pass proves the check runs, not
// that it refuses - so this plants each forbidden kind of file and
// requires a non-zero exit.
//
// It runs verify.sh against a directory rather than driving build.sh,
// and that is the reason the check lives in its own script. build.sh
// clears its staging directory before it starts, so anything planted
// beforehand is deleted before the check could ever see it - the first
// version of this test planted three files and watched all three vanish.
// A check that can only inspect files its own script copied is a check on
// the cp lines, not on the package.
func TestVerifyRefusesWhatMustNeverBePackaged(t *testing.T) {
	verify := filepath.Join(repoRoot(t), "release", "verify.sh")

	cases := []struct {
		name string
		path string
		what string
	}{
		{"a log", "bir-sey.log", "collected data"},
		{"a log in a directory", "logs/collector.log", "a whole log tree"},
		{"real configuration", "collector.toml", "a real config rather than an example"},
		{"bot data", "bot-data.json", "the fetched bot list (A10)"},
		{"a private key", "tls.key", "a credential"},
		{"a database dump", "yedek-dump.sql", "collected analytics"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := t.TempDir()
			plant := filepath.Join(stage, tc.path)
			if err := os.MkdirAll(filepath.Dir(plant), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plant, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}

			out, err := exec.Command(verify, stage).CombinedOutput()
			if err == nil {
				t.Fatalf("verify.sh packaged %s and said nothing:\n%s", tc.what, out)
			}
			if !strings.Contains(string(out), "refusing to package") {
				t.Errorf("verify.sh failed but not with the refusal, so %s may have been caught "+
					"by something unrelated:\n%s", tc.what, out)
			}
		})
	}
}

// TestVerifyAcceptsAPackageThatIsFine, so the test above is known to be
// measuring the plant rather than a script that refuses everything.
func TestVerifyAcceptsAPackageThatIsFine(t *testing.T) {
	root := repoRoot(t)
	stage := build(t, root, t.TempDir(), "v0.0.0-clean")

	out, err := exec.Command(filepath.Join(root, "release", "verify.sh"), stage).CombinedOutput()
	if err != nil {
		t.Fatalf("verify.sh refused a package this repository just built: %v\n%s", err, out)
	}
}

// TestVerifyNoticesATamperedFile. The checksums are in the package so
// somebody who downloaded it can check it; a verify that wrote them and
// never read them back would be a receipt nobody validates.
func TestVerifyNoticesATamperedFile(t *testing.T) {
	root := repoRoot(t)
	stage := build(t, root, t.TempDir(), "v0.0.0-tamper")

	// One byte appended to a shipped binary, which is what a tampered
	// download looks like.
	f, err := os.OpenFile(filepath.Join(stage, "bin", "collector"), os.O_APPEND|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out, err := exec.Command(filepath.Join(root, "release", "verify.sh"), stage).CombinedOutput()
	if err == nil {
		t.Fatalf("verify.sh accepted a package with a modified binary:\n%s", out)
	}
}

// TestTheUnitsAreValidAccordingToSystemd.
//
// Asserted by systemd rather than by a list written here, and the
// difference is not academic. The units first carried StartLimitBurst and
// StartLimitIntervalSec in [Service]; systemd moved those to [Unit] in
// v229 and *ignores* them in the wrong section rather than refusing the
// file. A hand-written check of directive names passed it happily - the
// names are real, only the section was wrong - and the rate limiting did
// nothing at all.
//
// That is the same shape as the finding this whole phase exists for: a
// documented line that looks right, is accepted, and has no effect. The
// only reliable reader of a systemd unit is systemd.
func TestTheUnitsAreValidAccordingToSystemd(t *testing.T) {
	verifier, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not on this machine")
	}

	// Timers as well as services. A timer is a unit file with its own
	// directive set, systemd is just as willing to accept a wrong one
	// silently, and crucible-upgrader.timer is the only thing that ever
	// starts the upgrader - a typo in it is an upgrade button that
	// reports success and never runs.
	units, err := filepath.Glob(filepath.Join(repoRoot(t), "release", "systemd", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != len(unitExpectations) {
		t.Fatalf("found %d unit files, and unitExpectations lists %d",
			len(units), len(unitExpectations))
	}

	for _, unit := range units {
		name := filepath.Base(unit)
		out, _ := exec.Command(verifier, "verify", unit).CombinedOutput()

		// The exit status is not the signal: systemd-analyze also fails
		// because ExecStart points at /opt, which does not exist on a
		// machine that has not installed the package. Those lines are
		// filtered and everything else is a real complaint.
		for _, line := range strings.Split(string(out), "\n") {
			switch {
			case strings.TrimSpace(line) == "",
				strings.Contains(line, "is not executable"),
				strings.Contains(line, "not found"),
				strings.Contains(line, "Failed to prepare"):
				continue
			}
			t.Errorf("%s: %s", name, strings.TrimSpace(line))
		}
	}
}

// TestOnlyTheCollectorMayBindPrivilegedPorts.
//
// The collector fronts the website, so it binds 80 and 443 and needs
// CAP_NET_BIND_SERVICE. The other three sit behind a reverse proxy on
// high ports and need no capability at all.
//
// Worth a test because the units were written by copying one another, and
// a capability copied along with the rest is an authority nobody decided
// to grant - the quiet way a hardened unit stops being hardened.
func TestOnlyTheCollectorMayBindPrivilegedPorts(t *testing.T) {
	units, err := filepath.Glob(filepath.Join(repoRoot(t), "release", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}

	for _, unit := range units {
		body, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(unit)
		hasCap := strings.Contains(string(body), "AmbientCapabilities=CAP_NET_BIND_SERVICE")

		if name == "crucible-collector.service" {
			if !hasCap {
				t.Error("the collector cannot bind 80/443; it fronts the website")
			}
			continue
		}
		if hasCap {
			t.Errorf("%s asks for CAP_NET_BIND_SERVICE and listens on a high port behind a proxy", name)
		}
		if !strings.Contains(string(body), "CapabilityBoundingSet=\n") {
			t.Errorf("%s does not drop every capability", name)
		}
	}
}

// unitExpectations is the half of the unit checks that is allowed to
// differ, with the reason each difference exists.
//
// The other half reads the directory. A unit file nobody listed here
// fails, and an entry naming a file that is gone fails - the same
// two-way shape as the CSRF exemptions and the deadcode allowlist,
// because a one-way list is how a new unit arrives carrying whatever it
// was copied from.
var unitExpectations = map[string]struct {
	// user is the account the unit runs as, matched exactly.
	//
	// Exactly, and that is not fussiness. This was
	// `strings.Contains(body, "User=crucible")`, which
	// "User=crucible-upgrader" satisfies - so the one unit in this
	// directory that deliberately runs as a *different* account would
	// have passed a check written to confirm they all run as the same
	// one. A substring test of a key=value line answers a question
	// nobody asked.
	user string

	// writes is true when the unit needs a writable path under
	// ProtectSystem=strict, with why.
	writes bool
	why    string
}{
	"crucible-collector.service": {user: "crucible", writes: true,
		why: "long-running; writes its log tree"},
	"crucible-beacon.service": {user: "crucible", writes: true,
		why: "long-running; writes its log tree"},
	"crucible-analytics-api.service": {user: "crucible", writes: true,
		why: "long-running; writes its log tree"},
	"crucible-panel.service": {user: "crucible", writes: true,
		why: "long-running; writes its log tree"},

	"crucible-upgrader.service": {
		// Its own account because upgrader.toml carries the only DSN in
		// the deployment that can run DDL, and `crucible` is the panel.
		// A panel that could read that file could rewrite its own
		// database.
		user: "crucible-upgrader",
		// Nothing writable, on purpose: it runs for a second at a time
		// and logs to the journal, so the whole filesystem stays
		// read-only for it.
		writes: false,
		why:    "one-shot; logs to the journal and writes only to the database over TCP",
	},

	// A timer has no [Service] section at all, so none of the directives
	// below apply to it. It is listed because the directory listing is
	// checked against this map, and a timer silently outside every check
	// is how the file that decides whether the upgrader ever runs stops
	// being looked at.
	"crucible-upgrader.timer": {},
}

// TestEveryUnitCarriesTheHardening. Written once and copied four times,
// which is exactly when one line goes missing from one file.
func TestEveryUnitCarriesTheHardening(t *testing.T) {
	required := []string{
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"PrivateTmp=true",
		"MemoryDenyWriteExecute=true",
		"RestrictSUIDSGID=true",
		"LockPersonality=true",
		"SystemCallFilter=@system-service",
	}

	units, err := filepath.Glob(filepath.Join(repoRoot(t), "release", "systemd", "*"))
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, unit := range units {
		name := filepath.Base(unit)
		seen[name] = true

		want, listed := unitExpectations[name]
		if !listed {
			t.Errorf("%s is in release/systemd and not in unitExpectations. "+
				"Add it with the account it runs as and whether it needs a "+
				"writable path - a unit that no check knows about is a unit "+
				"carrying whatever it was copied from", name)
			continue
		}
		if strings.HasSuffix(name, ".timer") {
			continue // no [Service] section; see the map's last entry
		}

		body, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range required {
			if !strings.Contains(string(body), line) {
				t.Errorf("%s is missing %s", name, line)
			}
		}

		if got, _ := directive(string(body), "User"); got != want.user {
			t.Errorf("%s runs as %q, and unitExpectations says %q", name, got, want.user)
		}

		// ProtectSystem=strict makes everything read-only, so a unit that
		// writes has to name where. One with neither would fail at its
		// first log line, on a customer's machine - and one that names a
		// path it does not need has widened itself for no reason.
		//
		// Presence, not value: `ReadWritePaths=` with nothing after it is
		// legal systemd and means "reset the list", so a value test would
		// read a unit that had had its writable paths deliberately
		// cleared as a unit that never named any.
		_, hasRW := directive(string(body), "ReadWritePaths")
		if hasRW != want.writes {
			t.Errorf("%s %s ReadWritePaths, and unitExpectations says it %s (%s)",
				name,
				map[bool]string{true: "has", false: "has no"}[hasRW],
				map[bool]string{true: "should", false: "should not"}[want.writes],
				want.why)
		}
	}

	for name := range unitExpectations {
		if !seen[name] {
			t.Errorf("unitExpectations lists %s, which is not in release/systemd. "+
				"A stale entry is how the next unit of that name inherits a "+
				"decision nobody made about it", name)
		}
	}
}

// directive returns the value of key= in a unit file, and whether the
// key was there at all.
//
// Written because the check above used strings.Contains and a prefix
// matched. Reads whole lines and compares the key exactly, so
// "User=crucible-upgrader" is not an answer to "User=crucible".
//
// The second return separates "not set" from "set to nothing", which are
// different states in systemd and were the same one here.
func directive(body, key string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// TestASignedPackageProvesWhoBuiltIt.
//
// # What this closes
//
// SHA256SUMS detects corruption. It says nothing about origin, because
// the build writes it and ships it inside the same tarball - anybody
// handing somebody a package hands them a matching list with it.
//
// That was tolerable while installing meant a person choosing to unpack
// an archive. It stops being tolerable the moment the panel can ask for
// an update, because then "install this" arrives over the network and a
// checksum the requester also supplied is not a check. Without a
// signature, a panel that can ask for an update is a panel that can run
// code, and the panel is the part of this system facing the internet.
//
// # Built, signed and checked, rather than asserted
//
// The chain has four links - build writes the sums, the tool signs them,
// the tool verifies them, verify.sh reports which - and each of them has
// its own unit test. This runs all four against a package this test
// built, because the failure that matters is two links that are each
// correct and disagree about what is signed.
func TestASignedPackageProvesWhoBuiltIt(t *testing.T) {
	root := repoRoot(t)

	// From the repository root: a test binary runs in its own package's
	// directory, and ./cmd/releasesign does not exist from release/.
	keygenCmd := exec.Command("go", "run", "./cmd/releasesign", "-keygen")
	keygenCmd.Dir = root
	keys, err := keygenCmd.Output()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	priv, pub := keyPair(t, string(keys))

	out := t.TempDir()
	cmd := exec.Command("./release/build.sh")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"VERSION=v0.0.0-signed", "OUT="+out, "CA_RELEASE_KEY="+priv)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build.sh with a signing key: %v\n%s", err, combined)
	}
	stage := filepath.Join(out, "crucible-analytic-v0.0.0-signed")

	sig := filepath.Join(stage, "SHA256SUMS.sig")
	if _, err := os.Stat(sig); err != nil {
		t.Fatalf("build.sh was given a key and wrote no signature: %v", err)
	}

	// The signature is deliberately *not* in SHA256SUMS: it is what
	// proves the list, and a list cannot vouch for its own proof. If it
	// were listed, verify.sh's checksum step would fail on every signed
	// package - which is a good way to find out this rule was broken.
	sums, err := os.ReadFile(filepath.Join(stage, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sums), "SHA256SUMS.sig") {
		t.Error("SHA256SUMS lists its own signature, which cannot be right in either " +
			"direction: signed before it exists, or listed after it was signed")
	}

	// verify.sh, told the key, must say it checked rather than say it
	// found one. The distinction is the whole point: "signed" is not
	// "verified", and a script that reported success on an unchecked
	// signature would be worse than one that ignored signatures.
	verify := exec.Command(filepath.Join(root, "release", "verify.sh"), stage)
	verify.Dir = root
	verify.Env = append(os.Environ(), "CA_RELEASE_PUBKEY="+pub)
	got, err := verify.CombinedOutput()
	if err != nil {
		t.Fatalf("verify.sh refused a package it just signed: %v\n%s", err, got)
	}
	if !strings.Contains(string(got), "signed and verified") {
		t.Errorf("verify.sh did not report that it checked the signature:\n%s", got)
	}

	// And the case that matters: one byte changed after signing.
	f, err := os.OpenFile(filepath.Join(stage, "SHA256SUMS"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	verify = exec.Command(filepath.Join(root, "release", "verify.sh"), stage)
	verify.Dir = root
	verify.Env = append(os.Environ(), "CA_RELEASE_PUBKEY="+pub)
	if got, err := verify.CombinedOutput(); err == nil {
		t.Fatalf("verify.sh accepted a SHA256SUMS edited after signing:\n%s", got)
	}
}

// TestAnUnsignedPackageSaysSoRatherThanLookingFine.
//
// A build with no key is the ordinary case - anybody checking that the
// bytes reproduce builds without one - so it must not fail. What it must
// not do either is stay quiet: an unsigned package is one the panel's
// update button refuses, and a customer's machine is the wrong place to
// discover that.
func TestAnUnsignedPackageSaysSoRatherThanLookingFine(t *testing.T) {
	root := repoRoot(t)

	out := t.TempDir()
	cmd := exec.Command("./release/build.sh")
	cmd.Dir = root
	// CA_RELEASE_KEY explicitly cleared: a maintainer running this suite
	// may have it set, and the test would then silently measure the
	// signed path while claiming to measure the other one.
	cmd.Env = append(os.Environ(), "VERSION=v0.0.0-unsigned", "OUT="+out, "CA_RELEASE_KEY=")
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build.sh without a signing key: %v\n%s", err, combined)
	}
	if !strings.Contains(string(combined), "unsigned") {
		t.Errorf("build.sh built an unsigned package without saying so:\n%s", combined)
	}

	stage := filepath.Join(out, "crucible-analytic-v0.0.0-unsigned")
	if _, err := os.Stat(filepath.Join(stage, "SHA256SUMS.sig")); err == nil {
		t.Error("a build with no key produced a signature")
	}
}

// keyPair pulls the two halves out of what -keygen printed.
//
// Parsed rather than passed around as fields, because the printed form
// is what a maintainer copies: a test that read the key some other way
// would keep passing after the output changed shape and left a person
// pasting the wrong line into upgrader.toml.
func keyPair(t *testing.T, printed string) (priv, pub string) {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		switch {
		case strings.HasPrefix(line, "CA_RELEASE_KEY="):
			priv = strings.TrimPrefix(line, "CA_RELEASE_KEY=")
		case strings.HasPrefix(line, "public_key"):
			if parts := strings.Split(line, `"`); len(parts) >= 2 {
				pub = parts[1]
			}
		}
	}
	if priv == "" || pub == "" {
		t.Fatalf("could not read both halves out of -keygen:\n%s", printed)
	}
	return priv, pub
}
