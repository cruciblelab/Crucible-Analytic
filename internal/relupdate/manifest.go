package relupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

// "Is there a newer version?", asked by the only component that can
// answer it honestly.
//
// # Why the panel does not ask this itself
//
// V5's update button made somebody type a version, and said why: the
// panel holds neither the address packages come from nor the key they
// are verified against. Both live in upgrader.toml, deliberately - a
// key an attacker with the database could change is a key that
// authorises nothing.
//
// A panel that fetched a version list would need the address, and the
// version it displayed would carry the panel's word rather than a
// signature. So the upgrader asks, verifies the answer against the key
// it already holds, and writes down what it found. The panel reads a
// row.
//
// # What the recorded answer is and is not
//
// It is a display fact and a prefilled field. It is *not* a shortcut
// past anything: installing a version still goes through Source.Fetch,
// which downloads the package, checks the signature over its SHA256SUMS
// and checks every file against that list. Nothing here shortens that,
// and a manifest that lied about which version exists would produce a
// download that fails to verify rather than an install of the wrong
// thing.
//
// That separation is the whole safety argument for letting a database
// row influence what a customer presses. The row chooses what is
// *offered*; the signature chain still decides what is *installed*.
//
// # The document
//
// A tiny text file, signed with the same key as a release's SHA256SUMS
// and a different domain separator - see releasesign.ManifestDomain for
// why that matters. Lines are "key: value", unknown keys are ignored so
// a later field cannot break an older reader, and the only required one
// is the version.
//
//	version: v0.21.0
//	released: 2026-09-04T05:00:00Z
//	notes: https://example.invalid/changelog#v0210

// ManifestName is the file the manifest lives in, under the base URL.
const ManifestName = "latest.txt"

// ManifestSigName is its detached signature.
const ManifestSigName = ManifestName + ".sig"

// maxManifest bounds the download. The document is three short lines; a
// megabyte is four orders of magnitude of room and still small enough
// that a server dribbling bytes cannot hold memory.
const maxManifest = 1 << 20

// ErrNoManifest means the base URL has no manifest. Distinguished from
// a failure because a deployment whose publisher has not started
// publishing one is not broken - it is one this feature does not work
// for yet, and the page should say so rather than showing an error.
var ErrNoManifest = errors.New("relupdate: the release source publishes no manifest")

// Manifest is the answer.
type Manifest struct {
	// Version is the latest published release.
	Version string
	// Released is when it was published, zero when the manifest did not
	// say. Optional because a publisher who omits it is not producing a
	// broken document, and the panel can draw the version without it.
	Released time.Time
	// Notes is where a human can read what changed, empty when absent.
	// Only http(s) URLs are kept; see parseManifest.
	Notes string
}

// CheckLatest fetches the manifest, verifies its signature and parses it.
//
// Verification before parsing, always. Parsing first would mean the
// fields of an unverified document had already been read, and "we only
// looked" is how a parser becomes the attack surface a signature was
// supposed to stand in front of.
func (s Source) CheckLatest(ctx context.Context) (Manifest, error) {
	if !s.PublicKey.IsSet() {
		// Refused rather than fetched-and-trusted. A source with no key
		// can still serve a file; what it cannot do is prove the file is
		// ours, and an unverified version string is exactly the value
		// this whole design refuses to put in front of a customer.
		return Manifest{}, releasesign.ErrNoKey
	}

	body, err := s.get(ctx, ManifestName, maxManifest)
	if errors.Is(err, errNotFound) {
		return Manifest{}, ErrNoManifest
	}
	if err != nil {
		return Manifest{}, err
	}
	sig, err := s.get(ctx, ManifestSigName, releasesign.SignatureSize*4)
	if errors.Is(err, errNotFound) {
		// A manifest with no signature is not a manifest. Saying so
		// rather than falling back to the unsigned one: a fallback here
		// would mean an attacker who could delete a file could also
		// remove the requirement to sign.
		return Manifest{}, fmt.Errorf("%w: %s is published without %s",
			releasesign.ErrBadSignature, ManifestName, ManifestSigName)
	}
	if err != nil {
		return Manifest{}, err
	}

	if err := s.PublicKey.VerifyIn(releasesign.ManifestDomain, body, sig); err != nil {
		return Manifest{}, fmt.Errorf("relupdate: the manifest at %s did not verify: %w",
			s.BaseURL, err)
	}
	return parseManifest(body)
}

// ParseManifest reads a manifest document.
//
// Exported so the signing tool can check a manifest before signing it:
// a publisher who signs a version this project could never install has
// made a document every deployment will fetch, verify and refuse, and
// they should find that out at the moment they sign rather than from a
// customer.
func ParseManifest(body []byte) (Manifest, error) { return parseManifest(body) }

// parseManifest reads the verified document.
//
// Unknown keys are ignored so that adding a field later cannot break a
// deployment running an older binary - the reader that must keep working
// is the one already installed, not the one being written.
func parseManifest(body []byte) (Manifest, error) {
	var out Manifest
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "version":
			out.Version = value
		case "released":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				out.Released = t.UTC()
			}
		case "notes":
			// http(s) only, and checked here rather than at the template.
			// The manifest is signed, so this is not about trusting the
			// publisher - it is about a signing key that leaks producing
			// a link that can do more than open a page.
			if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
				out.Notes = value
			}
		}
	}
	if !ValidVersion(out.Version) {
		return Manifest{}, fmt.Errorf("%w: the manifest names %q", ErrBadVersion, out.Version)
	}
	return out, nil
}

// errNotFound distinguishes "this source does not publish that" from
// "this source is broken", because the two produce different sentences
// on the page.
var errNotFound = errors.New("relupdate: not published")

// get fetches one small file from the base URL.
func (s Source) get(ctx context.Context, name string, limit int64) ([]byte, error) {
	target, err := s.fileURL(name)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("relupdate: fetching %s: %w", name, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("relupdate: fetching %s: the server answered %s",
			name, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("relupdate: reading %s: %w", name, err)
	}
	return body, nil
}

// fileURL is the address of one file directly under the base URL.
//
// The same https-only rule and the same escaping as PackageURL, because
// they are the same decision: this is the one place a name becomes part
// of a URL, and a base that is not https makes every check below it
// decorative.
func (s Source) fileURL(name string) (string, error) {
	if s.BaseURL == "" {
		return "", ErrNotConfigured
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil {
		return "", fmt.Errorf("relupdate: base_url: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("relupdate: base_url must be https, not %q", u.Scheme)
	}
	u.Path = path.Join(u.Path, url.PathEscape(name))
	return u.String(), nil
}
