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
		"schema/01-panel.sql", "schema/02-storage.sql",
		"schema/03-beacon.sql", "schema/04-asnlookup.sql",
		"schema/05-heartbeat.sql", "schema/06-retention.sql",
		"schema/07-logsink.sql", "schema/08-schemaver.sql",
		"systemd/crucible-collector.service", "systemd/crucible-beacon.service",
		"systemd/crucible-analytics-api.service", "systemd/crucible-panel.service",
		"ornek-yapilandirma/config.example.toml",
		"ornek-yapilandirma/beacon.example.toml",
		"ornek-yapilandirma/analytics-api.example.toml",
		"ornek-yapilandirma/panel.example.toml",
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
func TestEveryPackagedBinaryReportsItsVersion(t *testing.T) {
	const version = "v0.0.0-stamp"
	stage := build(t, repoRoot(t), t.TempDir(), version)

	for _, name := range []string{"collector", "beacon", "analytics-api", "panel", "devpass"} {
		out, err := exec.Command(filepath.Join(stage, "bin", name), "-version").Output()
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
				"and five identical lines answer nothing", name, got)
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

	units, err := filepath.Glob(filepath.Join(repoRoot(t), "release", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 4 {
		t.Fatalf("found %d units, want 4 (collector, beacon, analytics-api, panel)", len(units))
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

// TestEveryUnitCarriesTheHardening. Written once and copied three times,
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
		// ProtectSystem=strict makes everything read-only, so a service
		// that writes has to name where. A unit with neither would fail
		// at its first log line, on a customer's machine.
		"ReadWritePaths=",
		"User=crucible",
	}

	units, err := filepath.Glob(filepath.Join(repoRoot(t), "release", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		body, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range required {
			if !strings.Contains(string(body), line) {
				t.Errorf("%s is missing %s", filepath.Base(unit), line)
			}
		}
	}
}
