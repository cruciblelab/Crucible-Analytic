package relupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

// Fetching a release, which is the part of this that talks to the
// network and then writes executable files.
//
// # The order of the checks, which is the design
//
// Nothing is trusted until the signature says so, and nothing is
// executed until everything has been checked:
//
//  1. Build the address from configuration plus a version whose shape
//     was already checked. Nothing from the request row is interpolated
//     anywhere else.
//  2. Download to a temporary directory, under a size cap, over https.
//  3. Unpack, refusing any entry that would write outside that
//     directory or that is not a plain file or directory.
//  4. Verify the signature over SHA256SUMS.
//  5. Verify every file against SHA256SUMS.
//  6. Only then does the caller install anything.
//
// Steps 4 and 5 are both needed and neither implies the other. The
// signature says we wrote the list; the list says the files match it. A
// package with a valid signature over a list that does not describe its
// own files is a package somebody assembled from two of ours.

const (
	// maxPackage is the largest package this will download.
	//
	// 256 MB against a real package of about 40 MB. The cap is not a
	// guess at our own size, it is a bound on what a server that answers
	// this address can make the upgrader write to disk - and the disk it
	// writes to is the one holding the customer's analytics. Without it,
	// "the address answered" and "the address kept answering for six
	// hours" are the same code path.
	maxPackage = 256 << 20

	// maxFile bounds any single entry in the archive, which is what
	// stops one member expanding to fill the disk on its own.
	maxFile = 128 << 20

	// maxEntries bounds how many members are unpacked. A real package
	// carries about forty.
	maxEntries = 4096
)

// ErrNotConfigured means this deployment has no [release] section.
var ErrNotConfigured = errors.New("relupdate: no release source is configured")

// Source is where packages come from and how they are checked.
//
// Both fields come from upgrader.toml, and that is the whole point:
// neither is reachable from the panel's role, so a compromised panel can
// ask for a version and can change neither the address nor the key.
type Source struct {
	BaseURL   string
	PublicKey releasesign.PublicKey

	// Client is the HTTP client, so a test can supply one. Nil means a
	// client with a timeout, never http.DefaultClient - the default has
	// none, and a server that accepts a connection and then says nothing
	// would hold this until the process died.
	Client *http.Client

	// GOOS and GOARCH name the package to fetch. Empty means this
	// build's own, which is what production wants; a test sets them so
	// it does not depend on the machine it runs on.
	GOOS, GOARCH string
}

// PackageURL is the address a version lives at.
//
// Built by joining path segments rather than by formatting a string,
// so a version that somehow carried a slash could not climb out of the
// base URL's path. ValidVersion already refuses one; this is the second
// wall, and it is here because URL building is exactly where a single
// check is one too few.
func (s Source) PackageURL(version string) (string, error) {
	if s.BaseURL == "" {
		return "", ErrNotConfigured
	}
	if !ValidVersion(version) {
		return "", fmt.Errorf("%w: %q", ErrBadVersion, version)
	}

	u, err := url.Parse(s.BaseURL)
	if err != nil {
		return "", fmt.Errorf("relupdate: base_url: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("relupdate: base_url must be https, not %q", u.Scheme)
	}

	name := fmt.Sprintf("crucible-analytic-%s-%s-%s.tar.gz", version, s.goos(), s.goarch())
	// path.Join collapses any "." and ".." that survived, and
	// url.PathEscape stops a segment from introducing new ones.
	u.Path = path.Join(u.Path, url.PathEscape(version), url.PathEscape(name))
	return u.String(), nil
}

func (s Source) goos() string {
	if s.GOOS != "" {
		return s.GOOS
	}
	return runtimeGOOS
}

func (s Source) goarch() string {
	if s.GOARCH != "" {
		return s.GOARCH
	}
	return runtimeGOARCH
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	// Ten minutes: a 40 MB package over a link the customer is also
	// serving a website on. Long enough to be generous, bounded so a
	// server that stalls cannot hold the upgrader past the queue's own
	// StaleAfter.
	return &http.Client{Timeout: 10 * time.Minute}
}

// Fetch downloads a version into dir and returns the unpacked root.
//
// dir is emptied of nothing and created if absent; the caller owns it
// and is expected to hand over a fresh temporary directory, because
// everything written here is untrusted until the last step returns.
func (s Source) Fetch(ctx context.Context, version, dir string) (string, error) {
	if !s.PublicKey.IsSet() {
		// Before the download rather than after: a deployment with no
		// key cannot install anything, and spending ten minutes and 40
		// megabytes to discover that is a worse way to say so.
		return "", fmt.Errorf("%w (no public key)", ErrNotConfigured)
	}

	addr, err := s.PackageURL(version)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return "", fmt.Errorf("relupdate: request: %w", err)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("relupdate: fetching %s: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The address is in the message. An operator reading "404" with
		// no address cannot tell a wrong base_url from a version that
		// was never published, and those have different fixes.
		return "", fmt.Errorf("relupdate: %s answered %s", addr, resp.Status)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	root, err := unpack(io.LimitReader(resp.Body, maxPackage+1), dir)
	if err != nil {
		return "", err
	}
	if err := s.verify(root); err != nil {
		return "", err
	}
	return root, nil
}

// verify is steps 4 and 5: our signature over the list, then the list
// over the files.
func (s Source) verify(root string) error {
	sums, err := os.ReadFile(filepath.Join(root, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("relupdate: the package carries no SHA256SUMS: %w", err)
	}
	sig, err := os.ReadFile(filepath.Join(root, "SHA256SUMS.sig"))
	if err != nil {
		// Its own message. "Unsigned" and "wrongly signed" are different
		// problems: the first is usually a package built without a key,
		// the second is one somebody changed.
		return fmt.Errorf("relupdate: the package is unsigned: %w", err)
	}
	if err := s.PublicKey.Verify(sums, sig); err != nil {
		return fmt.Errorf("relupdate: %w", err)
	}
	return checkSums(root, string(sums))
}

// checkSums hashes every file the list names and compares.
//
// Every line, and a missing file is a failure rather than a skip: a
// package assembled from a signed list and half the files it describes
// would otherwise install cleanly and leave a deployment with binaries
// from two releases.
func checkSums(root, sums string) error {
	n := 0
	for _, line := range strings.Split(sums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			return fmt.Errorf("relupdate: SHA256SUMS line %d is not a checksum line", n+1)
		}
		name = strings.TrimPrefix(strings.TrimSpace(name), "./")

		// The list is signed, so a name in it is one we wrote - but this
		// runs before anything is installed and a path check that only
		// holds because the signature held is a check in the wrong
		// order. Cheap, and it means the two verifications are
		// independent.
		clean, err := safeJoin(root, name)
		if err != nil {
			return fmt.Errorf("relupdate: SHA256SUMS names %q, which is not inside the package", name)
		}

		got, err := hashFile(clean)
		if err != nil {
			return fmt.Errorf("relupdate: %s is named in SHA256SUMS and not in the package: %w", name, err)
		}
		if got != strings.TrimSpace(want) {
			return fmt.Errorf("relupdate: %s does not match SHA256SUMS", name)
		}
		n++
	}
	if n == 0 {
		// A signed empty list verifies perfectly and covers nothing.
		return errors.New("relupdate: SHA256SUMS is empty, so it vouches for no file at all")
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxFile)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safeJoin resolves name under root, and refuses rather than repairs.
//
// # Why it refuses a climbing path instead of absorbing one
//
// The first version cleaned the name against "/" first, which is the
// usual advice: Clean turns "pkg/../../../../etc/cron.d/evil" into
// "/etc/cron.d/evil", and joining that to root gives
// "root/etc/cron.d/evil" - inside the directory, so nothing escapes.
//
// The tests refused to accept that, and they were right. Nothing
// escaping is not the same as nothing wrong. Our build never produces a
// path with ".." in it, so an archive carrying one is not ours, and
// quietly rewriting it puts a file somewhere the archive did not ask
// for while reporting success. The top-level check would meanwhile read
// the first segment of a name whose file went elsewhere entirely.
//
// So: any ".." element is a refusal, not a repair. The escape check
// below stays as the second wall, because a single check in front of a
// path join is one too few and the consequence here is a file in a
// directory systemd reads.
func safeJoin(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("relupdate: refusing path %q", name)
	}
	// Both separators: a tar written on Windows carries backslashes, and
	// filepath on Linux does not treat one as a separator - so "a\..\b"
	// would be one filename here and a climbing path when unpacked
	// somewhere else.
	for _, elem := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if elem == ".." {
			return "", fmt.Errorf("relupdate: refusing path %q: it climbs out of the "+
				"package, and a release package never contains one", name)
		}
	}

	clean := filepath.Join(root, filepath.Clean("/"+name))
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relupdate: refusing path %q", name)
	}
	return clean, nil
}

// unpack extracts a .tar.gz into dir and returns the single top-level
// directory it contains.
//
// # What it refuses, and why each one
//
// Symlinks and hard links: a link is how an archive writes outside
// itself without any path in it containing "..". The package has none,
// so refusing them costs nothing and closes the whole class.
//
// Device nodes, fifos, setuid bits: nothing in a release needs them,
// and each is a way for an archive to leave something behind that is
// not a file.
//
// Absolute and climbing paths: the ordinary traversal, refused by
// safeJoin.
//
// Counts and sizes: a decompression bomb is a valid archive.
func unpack(r io.Reader, dir string) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("relupdate: the download is not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	top := ""
	entries := 0

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("relupdate: reading the archive: %w", err)
		}
		if entries++; entries > maxEntries {
			return "", fmt.Errorf("relupdate: the archive holds more than %d entries", maxEntries)
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		default:
			return "", fmt.Errorf("relupdate: the archive holds %q, which is neither a "+
				"file nor a directory. A release package contains neither links nor "+
				"device nodes, and both are ways to write outside the archive",
				hdr.Name)
		}

		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return "", err
		}

		// The top-level directory, which the package is named after.
		if first := firstSegment(hdr.Name); first != "" {
			switch {
			case top == "":
				top = first
			case top != first:
				return "", fmt.Errorf("relupdate: the archive has two top-level "+
					"directories, %q and %q; a release package has one", top, first)
			}
		}

		if hdr.Typeflag == tar.TypeDir {
			// 0700, not the archive's mode.
			//
			// This tree is the upgrader's private workspace and nothing
			// else has any business walking it: everything in it is
			// untrusted until verify() returns, and what finally gets
			// installed is copied out with modes the installer chooses.
			// Fetch already creates the top of it 0700; matching that
			// the whole way down means the answer does not depend on
			// which directory a reader happens to look at.
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", err
			}
			continue
		}
		if hdr.Size > maxFile {
			return "", fmt.Errorf("relupdate: %s is larger than %d bytes", hdr.Name, maxFile)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", err
		}
		// Mode from the archive, masked: the executable bit has to
		// survive or nothing runs, and setuid must not.
		//
		// Files keep 0755 while the directories above are 0700, which
		// is not an inconsistency: the directory modes decide who can
		// reach this tree, and nobody but the upgrader should. The file
		// mode is carried so the executable bit survives to the
		// installer, which is the one that decides what the binaries
		// look like where systemd runs them.
		mode := hdr.FileInfo().Mode().Perm() & 0o755
		if err := writeFile(target, tr, mode, hdr.Size); err != nil {
			return "", err
		}
	}

	if top == "" {
		return "", errors.New("relupdate: the archive has no top-level directory")
	}
	return filepath.Join(dir, top), nil
}

func writeFile(path string, r io.Reader, mode os.FileMode, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	// LimitReader on top of the declared size: a header that lies about
	// how big a member is must not be able to write more than it said.
	if _, err := io.Copy(f, io.LimitReader(r, size)); err != nil {
		return err
	}
	return f.Close()
}

func firstSegment(name string) string {
	name = strings.TrimPrefix(name, "./")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return name
}
