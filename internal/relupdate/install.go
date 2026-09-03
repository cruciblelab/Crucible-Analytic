package relupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Installing a verified package, and putting it back when it does not
// work.
//
// # Why this file is written defensively even though the package is ours
//
// By the time anything here runs, the package has been fetched over
// https, unpacked with every hostile shape refused, checked against our
// signature and checked file by file against its own list. What is left
// is genuinely ours.
//
// It can still be wrong. A release built for the wrong architecture, a
// binary that panics on a libc this machine does not have, a version
// with a startup bug we did not catch: all of those are packages we
// really signed. The signature says who made it, never that it works
// here.
//
// And the cost of finding out late is the customer's website. The
// collector stands in front of it; a collector that does not start takes
// the site down, and the panel that would let somebody press "undo" may
// be down beside it.
//
// # So the order is: check, then replace, then check again
//
//  1. Every new binary is run with -version, in the unpacked directory,
//     before anything is touched. Wrong architecture, truncated file,
//     missing library and startup panic all fail here - and failing here
//     costs nothing, because nothing has been replaced yet.
//  2. The current binaries are moved aside, not deleted.
//  3. The new ones are renamed into place.
//  4. Each installed binary is run with -version again.
//  5. Any failure in 3 or 4 puts the old ones back.
//
// Step 1 is what makes step 5 rare. A rollback is the path with the
// least testing in any system, because it runs only when something else
// has already gone wrong - so the design's job is to make the common
// failures happen before there is anything to roll back.
//
// # What -version proves, and what it does not
//
// It proves the file exists, is executable on this machine, links, and
// reaches its own main. That is exactly the set of failures a mismatched
// package produces, and it is not a substitute for the service actually
// serving. Whether a restarted collector is healthy is a bigger question
// than this file answers - see Installer.Restart.

// ErrNoBinaries means the package had no bin/ directory, which a real
// one always has.
var ErrNoBinaries = errors.New("relupdate: the package carries no binaries")

// Result is what happened, in the shape the queue records.
type Result struct {
	// Installed names the binaries that were replaced.
	Installed []string
	// RolledBack is true when the previous binaries were put back.
	RolledBack bool
	// Restarted names the services that were restarted, empty when
	// nothing was.
	Restarted []string
}

// Installer replaces the binaries under Prefix.
type Installer struct {
	// Prefix is the installation root; binaries live in Prefix/bin.
	Prefix string

	// Verify runs a binary and reports whether it works. Nil means
	// versionCheck, which runs it with -version.
	//
	// Injectable so the tests can make a binary fail on demand without
	// shipping a broken one, and so a caller with a better check than
	// "it prints its version" can supply one.
	Verify func(ctx context.Context, path string) error

	// Restart restarts the services after a successful install. Nil
	// means nothing is restarted, and that is the honest default.
	//
	// # Why this is a hook rather than a systemctl call
	//
	// Restarting a unit needs authority this process does not have: the
	// upgrader runs as crucible-upgrader, and the four services run as
	// crucible under units root owns. Granting it is a deployment
	// decision with a real blast radius - a process that can restart
	// services is a process that can stop them - so it is granted
	// explicitly, in a file an operator writes, or not at all.
	//
	// When it is nil the install still happens and the panel says a
	// restart is needed. That is a worse experience than a button that
	// finishes the job, and it is a better one than a component that
	// quietly acquired the power to stop the customer's website.
	Restart func(ctx context.Context) ([]string, error)

	// Now is time.Now, so a test can name the keep directory.
	Now func() time.Time
}

// Install replaces the binaries under Prefix with the ones in root/bin.
//
// root is the unpacked package, and it must be one Source.Fetch
// returned: this function does no verification of its own, because a
// second, weaker copy of the checks in fetch.go would be a second answer
// to "is this ours" and the weaker answer is the one that would
// eventually be consulted.
func (in Installer) Install(ctx context.Context, root string) (Result, error) {
	var res Result

	newBins, err := binariesIn(filepath.Join(root, "bin"))
	if err != nil {
		return res, err
	}
	if len(newBins) == 0 {
		return res, ErrNoBinaries
	}

	// ---- 1. every new binary runs, before anything is touched ----
	//
	// All of them, not the first failure's worth: an operator who is
	// told "collector is for the wrong architecture" and then, after
	// fixing nothing, "beacon is too" has been made to do the same
	// investigation twice.
	verify := in.Verify
	if verify == nil {
		verify = versionCheck
	}
	var refused []string
	for _, b := range newBins {
		if err := verify(ctx, filepath.Join(root, "bin", b)); err != nil {
			refused = append(refused, fmt.Sprintf("%s (%v)", b, err))
		}
	}
	if len(refused) > 0 {
		return res, fmt.Errorf("relupdate: refusing to install a package whose "+
			"binaries do not run on this machine, and nothing was replaced: %s",
			strings.Join(refused, "; "))
	}

	binDir := filepath.Join(in.Prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return res, err
	}

	// ---- 2. the current binaries move aside ----
	//
	// Beside the destination rather than in /tmp, so every rename below
	// is within one filesystem. A rollback that could fail with
	// "invalid cross-device link" is a rollback that fails exactly when
	// it is needed.
	now := in.Now
	if now == nil {
		now = time.Now
	}
	keep := filepath.Join(binDir, ".previous-"+now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(keep, 0o700); err != nil {
		return res, err
	}

	saved := map[string]string{} // name -> path it was saved to
	for _, b := range newBins {
		cur := filepath.Join(binDir, b)
		if _, err := os.Stat(cur); err != nil {
			// Not there yet: a new binary this release adds. Nothing to
			// save, and nothing to put back if this goes wrong - the
			// rollback below removes it instead.
			continue
		}
		to := filepath.Join(keep, b)
		if err := os.Rename(cur, to); err != nil {
			// Failing here is the safe direction: some binaries are
			// saved, none are replaced, so putting them back is the
			// whole recovery.
			rollback(saved, binDir, nil)
			return res, fmt.Errorf("relupdate: could not move %s aside, so nothing "+
				"was replaced: %w", b, err)
		}
		saved[b] = to
	}

	// ---- 3. the new ones go into place ----
	var added []string
	for _, b := range newBins {
		src := filepath.Join(root, "bin", b)
		dst := filepath.Join(binDir, b)
		if err := copyExecutable(src, dst); err != nil {
			rollback(saved, binDir, added)
			res.RolledBack = true
			return res, fmt.Errorf("relupdate: installing %s failed and the previous "+
				"binaries were put back: %w", b, err)
		}
		if _, wasThere := saved[b]; !wasThere {
			added = append(added, b)
		}
		res.Installed = append(res.Installed, b)
	}

	// ---- 4. and they still run where they now live ----
	//
	// Step 1 already ran them in the unpacked directory. This is not the
	// same question: the file has been copied, its mode has been set,
	// and it is now on the path a unit names. A check that only ran
	// before the move would not notice a copy that lost its executable
	// bit.
	for _, b := range res.Installed {
		if err := verify(ctx, filepath.Join(binDir, b)); err != nil {
			rollback(saved, binDir, added)
			res.Installed = nil
			res.RolledBack = true
			return res, fmt.Errorf("relupdate: %s does not run where it was installed "+
				"and the previous binaries were put back: %w", b, err)
		}
	}

	sort.Strings(res.Installed)

	// ---- 5. and only now, if anybody granted it, a restart ----
	if in.Restart != nil {
		names, err := in.Restart(ctx)
		if err != nil {
			// Deliberately not a rollback.
			//
			// The binaries are installed and each one runs. A restart
			// that failed leaves the old processes serving from the
			// inodes they opened, which is a working system - and
			// putting the files back underneath them would fix nothing
			// while making the next restart go backwards.
			//
			// So this is reported and the install stands. The queue row
			// carries the message and the panel shows it.
			return res, fmt.Errorf("relupdate: the binaries are installed and each "+
				"one runs, but restarting the services failed. The old processes are "+
				"still serving; restart them by hand: %w", err)
		}
		res.Restarted = names
	}
	return res, nil
}

// rollback puts the saved binaries back and removes the added ones.
//
// # Why this is the plainest function in the file
//
// It runs when something else has already failed, which means it runs on
// the path with the least testing in any system and the highest cost of
// a second failure. So it does one thing per file, ignores nothing
// silently but stops for nothing either, and calls nothing that can
// itself need a rollback: no downloads, no verification, no allocation
// beyond a rename.
//
// Errors are deliberately not returned. Every caller is already
// returning a failure, and a rollback error would replace the message
// that says what actually went wrong with one about the cleanup.
func rollback(saved map[string]string, binDir string, added []string) {
	// The ones we replaced go back.
	for name, from := range saved {
		_ = os.Remove(filepath.Join(binDir, name))
		_ = os.Rename(from, filepath.Join(binDir, name))
	}
	// The ones this release introduced were not there before, so
	// "putting it back" means taking it away.
	for _, name := range added {
		_ = os.Remove(filepath.Join(binDir, name))
	}
}

// copyExecutable writes src to dst, atomically, keeping it executable.
//
// Written beside dst and renamed, for the reason install.sh gives: Linux
// refuses to write to a running executable (ETXTBSY), and rename(2) over
// an open file is fine - the running process keeps its inode and the
// next start picks up the new one. It is also atomic, so dst is never
// half a binary.
//
// Copied rather than renamed from the package because the unpack
// directory is a temporary one that may be on another filesystem, and a
// cross-device rename fails.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, maxFile)); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The mode again after close: OpenFile's permission argument is
	// masked by umask, and an upgrader running under a umask of 077
	// would otherwise install binaries the service account cannot
	// execute.
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// versionCheck runs a binary with -version and reports whether it worked.
//
// Every binary in this project answers -version; release_test.go asserts
// it, from the package rather than from a list. So a file that does not
// is not one of ours, whatever the signature said about the archive it
// arrived in.
//
// Five seconds: printing a version string is immediate, and the failure
// this bounds is a binary that starts and then blocks - which is a
// broken binary that would otherwise hold the upgrader until the queue
// swept it.
func versionCheck(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	if err != nil {
		// The output is in the message. "exit status 1" alone sends
		// somebody to read a log that may not exist yet; the binary's
		// own complaint usually names the missing library.
		trimmed := strings.TrimSpace(string(out))
		if len(trimmed) > 200 {
			trimmed = trimmed[:200] + "..."
		}
		if trimmed == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimmed)
	}
	return nil
}

// binariesIn lists the executables in a directory.
func binariesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoBinaries
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
