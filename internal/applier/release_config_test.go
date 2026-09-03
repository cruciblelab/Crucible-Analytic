package applier

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

func aKey(t *testing.T) string {
	t.Helper()
	priv, err := releasesign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return priv.Public().String()
}

// TestHalfAReleaseConfigIsRefused.
//
// # Why the two halves are not independent settings
//
// A base_url with no public key is not a slightly weaker update path. It
// is a feature that downloads code over the network and installs it, and
// the signature was the only thing that was ever going to stop that. A
// deployment in that state looks configured and installs anything the
// address serves.
//
// So the file is refused at startup rather than worked around. The
// upgrader not starting is loud, recoverable and immediately traced to
// one line; the alternative is quiet and is discovered by whoever
// controls the address.
func TestHalfAReleaseConfigIsRefused(t *testing.T) {
	key := aKey(t)

	cases := []struct {
		what   string
		cfg    ReleaseConfig
		wantOK bool
		wantIn string
	}{
		{
			what:   "neither, which is the default",
			cfg:    ReleaseConfig{},
			wantOK: true,
		},
		{
			what:   "both, correctly",
			cfg:    ReleaseConfig{BaseURL: "https://sur.example/paketler", PublicKey: key},
			wantOK: true,
		},
		{
			what:   "an address with no key",
			cfg:    ReleaseConfig{BaseURL: "https://sur.example/paketler"},
			wantIn: "way to run code from the network",
		},
		{
			what:   "a key with no address",
			cfg:    ReleaseConfig{PublicKey: key},
			wantIn: "nothing can be fetched",
		},
		{
			what:   "plain http",
			cfg:    ReleaseConfig{BaseURL: "http://sur.example/paketler", PublicKey: key},
			wantIn: "must be https",
		},
		{
			what:   "a scheme that is not http at all",
			cfg:    ReleaseConfig{BaseURL: "file:///paketler", PublicKey: key},
			wantIn: "must be https",
		},
		{
			what:   "no host",
			cfg:    ReleaseConfig{BaseURL: "https:///paketler", PublicKey: key},
			wantIn: "no host",
		},
		{
			what:   "a key that is not a key",
			cfg:    ReleaseConfig{BaseURL: "https://sur.example/p", PublicKey: "hayir"},
			wantIn: "public_key",
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.wantOK {
				if err != nil {
					t.Fatalf("this is a supported configuration and it was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("this was accepted, and a deployment in this state either " +
					"installs unsigned code or believes it can update and cannot")
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("the message does not say %q, so the reader has to guess "+
					"which half is wrong:\n  %v", c.wantIn, err)
			}
		})
	}
}

// TestAnUnconfiguredKeyVerifiesNothing.
//
// Key() returns a zero PublicKey when nothing is configured, and a zero
// key refuses every signature. The direction matters: a caller that
// forgets to check Configured() must end up refusing packages, not
// accepting them.
func TestAnUnconfiguredKeyVerifiesNothing(t *testing.T) {
	var none ReleaseConfig
	if none.Configured() {
		t.Error("an empty [release] reports itself configured")
	}
	if none.Key().IsSet() {
		t.Fatal("an empty [release] produced a usable key, which would be a key of zeroes")
	}

	// And a malformed key does not become a working one either. Validate
	// catches this at startup; Key() is what runs afterwards, and it must
	// not disagree.
	broken := ReleaseConfig{BaseURL: "https://sur.example/p", PublicKey: "hayir"}
	if broken.Key().IsSet() || broken.Configured() {
		t.Error("a malformed public key was turned into a usable one")
	}

	good := ReleaseConfig{BaseURL: "https://sur.example/p", PublicKey: aKey(t)}
	if !good.Configured() || !good.Key().IsSet() {
		t.Error("a correct [release] does not report itself configured, so the update " +
			"section would never appear")
	}
}
