// Package buildinfo answers one question from a shell: which build is
// this.
//
// It is the question support asks first, and until G2 four of the five
// binaries could not answer it. KURULUM.md told operators to build all
// five with
//
//	go build -ldflags "-X main.version=$VERSION" ./cmd/$b
//
// and `main.version` existed only in cmd/panel. The Go linker does not
// warn when -X names a symbol that is not there; it silently does
// nothing. So the documented command was right and four of its five
// iterations had no effect - measured with `go tool nm`, not assumed.
//
// # Why the stamp is not the only source
//
// The linker stamp carries what a person means by a version: a tag name,
// "v0.4.1", something to say out loud. Go itself already embeds the VCS
// revision and a dirty flag into every build made from a working tree,
// with no flags at all.
//
// Using only the stamp would mean an unstamped build says nothing, even
// though the binary knows exactly which commit it came from. Using only
// the embedded data would mean throwing away the tag. So: the stamp when
// there is one, the revision when there is not, and "unknown" only when
// the binary genuinely has neither - which happens when it was built
// with -buildvcs=false outside a repository.
package buildinfo

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Unknown is what Version reports when the binary carries neither a
// linker stamp nor embedded VCS data.
//
// A word rather than an empty string: an operator reading "version:" with
// nothing after it cannot tell whether the build is unstamped or the
// command printed badly.
const Unknown = "unknown"

// Version reports this build's version.
//
// stamped is the main package's version variable, set by
// -X main.version=... and empty when it was not.
func Version(stamped string) string {
	if v := strings.TrimSpace(stamped); v != "" {
		return v
	}
	if rev, modified, ok := vcs(); ok {
		if modified {
			// Said out loud rather than hidden. A binary built from a
			// dirty tree does not correspond to any commit, so reporting
			// the revision alone would name a commit whose source is not
			// what is running.
			return rev + "-dirty"
		}
		return rev
	}
	return Unknown
}

// vcs reads the revision Go embeds into builds made from a working tree.
func vcs() (revision string, modified, ok bool) {
	info, available := debug.ReadBuildInfo()
	if !available {
		return "", false, false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "", false, false
	}
	// Short form: a full hash is forty characters an operator has to read
	// aloud to somebody on a phone.
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision, modified, true
}

// Line is what -version prints: the binary's name, its version, and the
// toolchain and platform it was built for.
//
// One line, because it gets pasted into a support conversation. The Go
// version and platform are on it because "which build" and "built how"
// are the same question when a report is about a crash.
func Line(name, stamped string) string {
	return fmt.Sprintf("%s %s (%s %s/%s)",
		name, Version(stamped), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Print writes Line to w.
func Print(w io.Writer, name, stamped string) {
	fmt.Fprintln(w, Line(name, stamped))
}
