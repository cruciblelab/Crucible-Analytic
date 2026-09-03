package relupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in for a binary: a shell script that prints something and
// exits with a chosen code.
//
// Real executables rather than a mocked runner, because what is being
// tested includes the executable bit surviving a copy and a rename, and
// a fake that only records calls would pass while the file arrived
// unrunnable.
func writeBinary(t *testing.T, dir, name, says string, exit int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho " + says + "\nexit " + itoa(exit) + "\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// aPackage builds an unpacked tree with the named binaries.
func aPackage(t *testing.T, says string, exit int, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		writeBinary(t, filepath.Join(root, "bin"), n, says, exit)
	}
	return root
}

// anInstallation builds a prefix that already has binaries in it.
func anInstallation(t *testing.T, says string, names ...string) string {
	t.Helper()
	prefix := t.TempDir()
	for _, n := range names {
		writeBinary(t, filepath.Join(prefix, "bin"), n, says, 0)
	}
	return prefix
}

func run(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestAGoodPackageReplacesTheBinaries is the happy path, first so the
// rollbacks below mean something.
func TestAGoodPackageReplacesTheBinaries(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel", "collector")
	root := aPackage(t, "yeni", 0, "panel", "collector")

	res, err := Installer{Prefix: prefix}.Install(context.Background(), root)
	if err != nil {
		t.Fatalf("a good package was refused: %v", err)
	}
	if len(res.Installed) != 2 {
		t.Errorf("installed %v, wanted both", res.Installed)
	}
	if res.RolledBack {
		t.Error("a successful install reports a rollback")
	}

	for _, b := range []string{"panel", "collector"} {
		got := run(t, filepath.Join(prefix, "bin", b))
		if !strings.Contains(got, "yeni") {
			t.Errorf("%s is still the old one: %q", b, got)
		}
		info, err := os.Stat(filepath.Join(prefix, "bin", b))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s installed as %v, which systemd cannot execute", b, info.Mode().Perm())
		}
	}
}

// TestABinaryThatDoesNotRunIsRefusedBeforeAnythingIsTouched.
//
// This is the case the whole design is arranged around, and the
// assertion that matters is the second one: nothing was replaced.
//
// A package built for the wrong architecture, truncated in transit, or
// linked against a library this machine does not have is a package we
// really signed - the signature says who made it, never that it works
// here. Catching it before the first rename means there is nothing to
// roll back, and a rollback is the least-tested path in any system
// because it only runs when something else already failed.
func TestABinaryThatDoesNotRunIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel", "collector")
	root := aPackage(t, "yeni", 0, "panel")
	// One of the two does not run: the shape of a package for the wrong
	// architecture, where every file is present and one cannot execute.
	writeBinary(t, filepath.Join(root, "bin"), "collector", "exec format error", 1)

	res, err := Installer{Prefix: prefix}.Install(context.Background(), root)
	if err == nil {
		t.Fatal("a package with a binary that does not run was installed")
	}
	if !strings.Contains(err.Error(), "collector") {
		t.Errorf("the message does not name which binary was refused:\n  %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was replaced") {
		t.Errorf("the message does not say the installation is untouched, which is "+
			"the first thing the reader needs:\n  %v", err)
	}
	if res.RolledBack {
		t.Error("it reports a rollback, and there was nothing to roll back")
	}

	for _, b := range []string{"panel", "collector"} {
		if got := run(t, filepath.Join(prefix, "bin", b)); !strings.Contains(got, "eski") {
			t.Errorf("%s was replaced despite the refusal: %q", b, got)
		}
	}
}

// TestEveryBinaryIsNamedRatherThanTheFirstOne.
//
// An operator told "collector is for the wrong architecture" who fixes
// nothing and is then told "and beacon is too" has done the same
// investigation twice.
func TestEveryBinaryIsNamedRatherThanTheFirstOne(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel", "collector", "beacon")
	root := aPackage(t, "yeni", 0, "panel")
	writeBinary(t, filepath.Join(root, "bin"), "collector", "bozuk", 1)
	writeBinary(t, filepath.Join(root, "bin"), "beacon", "bozuk", 1)

	_, err := Installer{Prefix: prefix}.Install(context.Background(), root)
	if err == nil {
		t.Fatal("this was installed")
	}
	for _, want := range []string{"collector", "beacon"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message stops before naming %s:\n  %v", want, err)
		}
	}
}

// TestAFailureAfterTheFirstRenamePutsEverythingBack.
//
// The rollback proper. Verify is made to pass in the package and fail
// once the file is in place, which is the one failure the pre-flight
// cannot catch: a copy that arrived and does not work from where it now
// lives.
func TestAFailureAfterTheFirstRenamePutsEverythingBack(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel", "collector")
	root := aPackage(t, "yeni", 0, "panel", "collector")
	binDir := filepath.Join(prefix, "bin")

	in := Installer{
		Prefix: prefix,
		Verify: func(_ context.Context, path string) error {
			if strings.HasPrefix(path, binDir) {
				return errors.New("it does not run from here")
			}
			return nil
		},
	}

	res, err := in.Install(context.Background(), root)
	if err == nil {
		t.Fatal("a binary that fails after installation was accepted")
	}
	if !res.RolledBack {
		t.Error("the result does not report a rollback, so the panel would say " +
			"'failed' without saying what is running now")
	}
	if !strings.Contains(err.Error(), "put back") {
		t.Errorf("the message does not say the previous binaries were restored:\n  %v", err)
	}

	for _, b := range []string{"panel", "collector"} {
		got := run(t, filepath.Join(binDir, b))
		if !strings.Contains(got, "eski") {
			t.Fatalf("%s was left as the new one after a rollback: %q.\n"+
				"A rollback that leaves half the release in place is worse than no "+
				"rollback: nobody can tell which version is running", b, got)
		}
	}
}

// TestABinaryThisReleaseAddsIsRemovedOnRollback.
//
// The asymmetric half of a rollback, and the one that is easy to forget:
// a release that introduces a new binary has nothing to put back for it,
// so restoring means taking it away. Left behind, it is a file from a
// version that was never installed, sitting where a unit might name it.
func TestABinaryThisReleaseAddsIsRemovedOnRollback(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel")
	root := aPackage(t, "yeni", 0, "panel", "yeni-servis")
	binDir := filepath.Join(prefix, "bin")

	in := Installer{
		Prefix: prefix,
		Verify: func(_ context.Context, path string) error {
			if strings.HasPrefix(path, binDir) {
				return errors.New("no")
			}
			return nil
		},
	}
	if _, err := in.Install(context.Background(), root); err == nil {
		t.Fatal("this was accepted")
	}

	if _, err := os.Stat(filepath.Join(binDir, "yeni-servis")); !os.IsNotExist(err) {
		t.Errorf("a binary this release introduced survived the rollback (%v). "+
			"It is a file from a version that was never installed", err)
	}
	if got := run(t, filepath.Join(binDir, "panel")); !strings.Contains(got, "eski") {
		t.Errorf("panel was not restored: %q", got)
	}
}

// TestThePreviousBinariesAreKeptOnTheSameFilesystem.
//
// Not a tidiness point. Every rename in the rollback is within one
// directory, and a rollback that could fail with "invalid cross-device
// link" is a rollback that fails exactly when it is needed - which is
// after something else has already gone wrong, on a customer's machine,
// with the site possibly down.
func TestThePreviousBinariesAreKeptOnTheSameFilesystem(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel")
	root := aPackage(t, "yeni", 0, "panel")

	if _, err := (Installer{Prefix: prefix}).Install(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(prefix, "bin")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	keep := ""
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".previous-") {
			keep = filepath.Join(binDir, e.Name())
		}
	}
	if keep == "" {
		t.Fatalf("the previous binaries were not kept anywhere under %s.\n"+
			"Without them a failure after the first rename has nothing to restore", binDir)
	}
	if got := run(t, filepath.Join(keep, "panel")); !strings.Contains(got, "eski") {
		t.Errorf("what was kept is not the previous binary: %q", got)
	}
}

// TestARestartFailureDoesNotRollBack.
//
// The binaries are installed and each one runs. A restart that failed
// leaves the old processes serving from the inodes they already opened,
// which is a working system - and putting the files back underneath them
// would fix nothing while making the next restart go backwards.
//
// So the install stands and the message says what a person has to do.
func TestARestartFailureDoesNotRollBack(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel")
	root := aPackage(t, "yeni", 0, "panel")

	in := Installer{
		Prefix: prefix,
		Restart: func(context.Context) ([]string, error) {
			return nil, errors.New("systemctl: access denied")
		},
	}
	res, err := in.Install(context.Background(), root)
	if err == nil {
		t.Fatal("a failed restart was reported as a clean success")
	}
	if res.RolledBack {
		t.Fatal("a failed restart rolled the binaries back. The old processes are " +
			"still serving from the inodes they opened, so this fixes nothing and " +
			"makes the next restart go backwards")
	}
	if got := run(t, filepath.Join(prefix, "bin", "panel")); !strings.Contains(got, "yeni") {
		t.Errorf("the new binary was not left in place: %q", got)
	}
	if !strings.Contains(err.Error(), "by hand") {
		t.Errorf("the message does not tell the reader what to do:\n  %v", err)
	}
}

// TestNothingIsRestartedUnlessSomebodyGrantedIt.
//
// The default is nil, and nil means nothing is restarted. A component
// that can restart services is a component that can stop the customer's
// website, so that authority is granted in a file an operator writes or
// it is not held at all.
func TestNothingIsRestartedUnlessSomebodyGrantedIt(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel")
	root := aPackage(t, "yeni", 0, "panel")

	res, err := Installer{Prefix: prefix}.Install(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restarted) != 0 {
		t.Errorf("it restarted %v without being given a Restart function", res.Restarted)
	}
}

// TestAPackageWithNoBinariesIsRefused.
func TestAPackageWithNoBinariesIsRefused(t *testing.T) {
	prefix := anInstallation(t, "eski", "panel")

	for what, root := range map[string]string{
		"no bin directory": t.TempDir(),
		"an empty one":     emptyBin(t),
	} {
		t.Run(what, func(t *testing.T) {
			_, err := Installer{Prefix: prefix}.Install(context.Background(), root)
			if !errors.Is(err, ErrNoBinaries) {
				t.Fatalf("answered %v, want ErrNoBinaries", err)
			}
			if got := run(t, filepath.Join(prefix, "bin", "panel")); !strings.Contains(got, "eski") {
				t.Error("the existing installation was disturbed")
			}
		})
	}
}

func emptyBin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestTheVersionCheckReportsWhatTheBinarySaid.
//
// "exit status 1" alone sends somebody to read a log that may not exist
// yet. The binary's own complaint usually names the missing library,
// which is the whole answer.
func TestTheVersionCheckReportsWhatTheBinarySaid(t *testing.T) {
	dir := t.TempDir()
	path := writeBinary(t, dir, "bozuk", "libpq.so.5: cannot open shared object file", 1)

	err := versionCheck(context.Background(), path)
	if err == nil {
		t.Fatal("a binary that exits non-zero was reported as working")
	}
	if !strings.Contains(err.Error(), "libpq") {
		t.Errorf("the message drops what the binary said, which is usually the "+
			"whole answer:\n  %v", err)
	}

	good := writeBinary(t, dir, "saglam", "v0.20.0", 0)
	if err := versionCheck(context.Background(), good); err != nil {
		t.Errorf("a binary that answers -version was reported as broken: %v", err)
	}

	if err := versionCheck(context.Background(), filepath.Join(dir, "yok")); err == nil {
		t.Error("a binary that is not there was reported as working")
	}
}
