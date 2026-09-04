// releasesign signs a release package, and makes the keys to do it.
//
// A maintainer's tool. It is not shipped in the package and no service
// calls it - the customer's machine only ever verifies, and verification
// lives in internal/releasesign where the upgrader can reach it.
//
//	releasesign -keygen
//	releasesign -sign dist/crucible-analytic-v0.20.0/SHA256SUMS
//
// # Why the key is read from the environment
//
// A signing key on a command line is a signing key in a shell history,
// in the process list, and in whatever collects both. CA_RELEASE_KEY is
// not private either - environments leak - but it is the least bad of
// the ways a script can hand a secret to a program, and the alternative
// this replaces is a maintainer pasting the key into a terminal.
//
// There is no -key flag on purpose. A flag that exists gets used.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

func main() {
	keygen := flag.Bool("keygen", false, "make a new signing key and print both halves")
	sign := flag.String("sign", "", "path to a SHA256SUMS to sign; writes SHA256SUMS.sig beside it")
	verify := flag.String("verify", "", "path to a SHA256SUMS to check against CA_RELEASE_PUBKEY")
	manifest := flag.String("sign-manifest", "",
		"path to a latest.txt to sign; writes latest.txt.sig beside it")
	flag.Parse()

	chosen := 0
	for _, on := range []bool{*keygen, *sign != "", *verify != "", *manifest != ""} {
		if on {
			chosen++
		}
	}
	switch {
	case chosen > 1:
		fail("-keygen, -sign, -sign-manifest and -verify do different jobs; run one of them")
	case *keygen:
		if err := generate(); err != nil {
			fail("%v", err)
		}
	case *sign != "":
		if err := signFile(*sign); err != nil {
			fail("%v", err)
		}
	case *verify != "":
		if err := verifyFile(*verify); err != nil {
			fail("%v", err)
		}
	case *manifest != "":
		if err := signManifest(*manifest); err != nil {
			fail("%v", err)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// verifyFile is the check a customer can run before installing, and the
// one release/verify.sh runs on the way out.
//
// Deliberately the same code path the upgrader will use: a verifier that
// agrees with the real one only by inspection is a verifier that will
// one day disagree with it.
func verifyFile(path string) error {
	raw := os.Getenv("CA_RELEASE_PUBKEY")
	if raw == "" {
		return errors.New("set CA_RELEASE_PUBKEY to the public key from the release page")
	}
	pub, err := releasesign.ParsePublicKey(raw)
	if err != nil {
		return fmt.Errorf("CA_RELEASE_PUBKEY: %w", err)
	}

	sums, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(path + ".sig")
	if err != nil {
		// Named as its own condition. "No signature" and "wrong
		// signature" are different problems with different answers, and
		// a reader given one message for both will try the wrong fix.
		return fmt.Errorf("%s carries no signature: %w", filepath.Base(path), err)
	}
	if err := pub.Verify(sums, sig); err != nil {
		return err
	}
	fmt.Printf("%s verifies against %s\n", filepath.Base(path), pub.String())
	return nil
}

// generate prints a new pair.
//
// Both halves, with a line saying which is which and where each one
// goes. A tool that prints two hex strings and leaves the reader to work
// out which is the secret is a tool that publishes a signing key exactly
// once.
func generate() error {
	priv, err := releasesign.Generate()
	if err != nil {
		return err
	}
	fmt.Printf(`# The signing key. Keep it. Anybody holding it can sign a package
# this deployment will install without complaint. It never goes on a
# customer's machine and never into this repository.
CA_RELEASE_KEY=%s

# The public half. This is what goes in upgrader.toml, and it is safe to
# publish - put it on the release page so somebody can check a package
# without asking us.
[release]
public_key = "%s"
`, priv.String(), priv.Public().String())
	return nil
}

// signFile signs a SHA256SUMS and writes the signature beside it.
// signManifest signs the "which version is current" document.
//
// Its own mode rather than a flag on -sign, because the two documents
// are signed in different domains and the whole point of that is that
// they cannot be confused. A single mode with a switch would put the
// choice one typo away from producing a signature that verifies as the
// wrong kind of thing.
//
// The manifest is parsed before it is signed. A publisher who signs a
// version string this project could never install has produced a
// document that every deployment will fetch, verify and then refuse -
// and they will find out from a customer rather than from here.
func signManifest(path string) error {
	raw := os.Getenv("CA_RELEASE_KEY")
	if raw == "" {
		return errors.New("set CA_RELEASE_KEY to the signing key (releasesign -keygen makes one)")
	}
	priv, err := releasesign.ParsePrivateKey(raw)
	if err != nil {
		return fmt.Errorf("CA_RELEASE_KEY: %w", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := relupdate.ParseManifest(body); err != nil {
		return fmt.Errorf("%s is not a manifest this project could read: %w", path, err)
	}

	sig, err := priv.SignIn(releasesign.ManifestDomain, body)
	if err != nil {
		return err
	}
	out := path + ".sig"
	if err := os.WriteFile(out, sig, 0o644); err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func signFile(path string) error {
	raw := os.Getenv("CA_RELEASE_KEY")
	if raw == "" {
		return errors.New("set CA_RELEASE_KEY to the signing key (releasesign -keygen makes one)")
	}
	priv, err := releasesign.ParsePrivateKey(raw)
	if err != nil {
		return fmt.Errorf("CA_RELEASE_KEY: %w", err)
	}

	sums, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(sums) == 0 {
		// A zero-byte SHA256SUMS signs perfectly well and verifies
		// perfectly well, and it covers nothing at all. Caught here
		// because the only other place it would show up is a customer
		// installing an empty package.
		return fmt.Errorf("%s is empty; there is nothing to sign", path)
	}

	sig, err := priv.Sign(sums)
	if err != nil {
		return err
	}

	// Beside the sums, named after them. The upgrader finds it by
	// appending ".sig" rather than by being told, so the two names are
	// decided in one place.
	out := path + ".sig"
	if err := os.WriteFile(out, sig, 0o644); err != nil {
		return err
	}

	// Verified before reporting success, with the public half derived
	// from the key that just signed. It costs microseconds and it closes
	// the one failure this tool could otherwise ship: a signature that
	// is written but does not verify, discovered by a customer.
	if err := priv.Public().Verify(sums, sig); err != nil {
		return fmt.Errorf("wrote %s and it does not verify: %w", out, err)
	}

	fmt.Printf("signed %s\n  -> %s\n  public key %s\n",
		filepath.Base(path), out, priv.Public().String())
	return nil
}

// fail prints one line and stops.
//
// The prefix is dropped when the message already carries it, because
// internal/releasesign names itself in its own errors and the first
// version of this printed "releasesign: releasesign: the signature does
// not match". A doubled prefix reads as a program that lost track of
// what it was doing, on the one line somebody sees when an install is
// refused.
func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(msg, "releasesign: ") {
		msg = "releasesign: " + msg
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
