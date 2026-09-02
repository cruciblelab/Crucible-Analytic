package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/collector"
)

// privacy.ip_storage is one setting written into two config files, and
// the two services that read it have to agree about what it may say.
//
// # What went wrong
//
// They did not agree, and a comment is what caused it. The beacon's
// PrivacyConfig documented the setting as `"masked" (the default),
// "full" or "hashed"`, and internal/beacon/schema.sql carried a whole
// block explaining "hashed" mode - that ip is left NULL and ip_hash
// carries HMAC(key, masked_ip) instead.
//
// None of that is true any more. internal/privacy has exactly two
// modes, and full mode stores the masked address *and* a token derived
// from the whole one; ip is never NULL and the crossover join never
// moves. The comment described a design that was replaced.
//
// The cost was not the stale prose. An operator following that comment
// writes ip_storage = "hashed", and then:
//
//   - the collector refuses to start, naming the two legal values;
//   - the beacon starts, silently falls back to masked, and says
//     nothing.
//
// So the two writers of the crossover join could be configured with one
// file, one key, one intent - and one of them would be running while the
// other was refusing to. The failure arrives as "the collector will not
// start" and the actual defect is a doc comment three files away.
//
// # Why the list is not derived from a single place
//
// One side of this mirror is read out of internal/privacy: the modes it
// actually declares. The other is the hand list below, with a reason per
// entry, because the interesting probes are the values that are *not*
// modes - the ones somebody plausibly writes. No derivation can produce
// those; a person has to have thought of them.
// A key long enough to satisfy privacy.MinHashKeyLen. Its content does
// not matter here; its length does.
const probeKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var ipModeProbes = []struct {
	name  string
	value string
	key   string
	valid bool
	why   string
}{
	{"absent", "", "", true,
		"every config file written before the setting existed, and every one that " +
			"simply does not mention it"},
	{"masked", "masked", "", true, "the default, and it needs no key"},
	{"full with a key", "full", probeKey, true, "the other real mode, configured properly"},
	{"full with no key", "full", "", false,
		"the quiet one: privacy.TokenIP returns nil rather than tokenising weakly, so " +
			"this runs in \"full\" mode writing no tokens at all while the config file " +
			"and the panel both say full"},
	{"full with a short key", "full", "too-short", false,
		"same failure, and the shape somebody actually produces - a key typed by hand " +
			"instead of generated"},
	{"hashed", "hashed", "", false,
		"the mode two doc comments invited and no code has implemented since full mode " +
			"replaced it - this is the value that split the two services"},
	{"tam", "tam", "", false,
		"the Turkish word for full; the panel speaks Turkish and the config file does not"},
	{"wrong case", "MASKED", "", false,
		"right word, wrong case - accepted by one service and not the other is the " +
			"worst outcome, so both must refuse"},
	{"none", "none", "", false,
		"what somebody writes meaning 'store no address', which is not a mode: masked " +
			"is already the floor"},
}

// ipModeConst finds the string literal behind each IPMode constant.
var ipModeConst = regexp.MustCompile(`(?m)^\tIP[A-Za-z]+ IPMode = "([a-z]+)"$`)

// TestTheTwoWritersAgreeAboutPrivacyIPStorage.
//
// Both configs are loaded from real files through their real exported
// loaders, rather than by building a struct and calling an unexported
// validate. That is the path an operator takes, and it is the path where
// the disagreement showed: the file is the artefact the two services
// share.
func TestTheTwoWritersAgreeAboutPrivacyIPStorage(t *testing.T) {
	for _, probe := range ipModeProbes {
		t.Run(probe.name, func(t *testing.T) {
			dir := t.TempDir()

			beaconErr := loadBeaconWith(t, dir, probe.value, probe.key)
			collectorErr := loadCollectorWith(t, dir, probe.value, probe.key)

			beaconOK, collectorOK := beaconErr == nil, collectorErr == nil
			if beaconOK != collectorOK {
				t.Fatalf("the two writers disagree about ip_storage = %q (%s).\n"+
					"beacon: %v\ncollector: %v\n"+
					"They write the two columns the crossover join compares, so one "+
					"starting while the other refuses is a deployment that looks "+
					"half-configured for a reason neither service names",
					probe.value, probe.why, beaconErr, collectorErr)
			}
			if beaconOK != probe.valid {
				t.Errorf("ip_storage = %q was %s and the list says it should be %s (%s)",
					probe.value, accepted(beaconOK), accepted(probe.valid), probe.why)
			}
		})
	}
}

// TestEveryPrivacyModeIsProbed keeps the list above from going stale.
//
// A third mode added to internal/privacy would be accepted by whichever
// service was taught about it, and this file would carry on testing the
// two it already knows - which is the shape of failure this package
// exists for.
func TestEveryPrivacyModeIsProbed(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "privacy", "ip.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := ipModeConst.FindAllStringSubmatch(string(body), -1)
	if len(found) < 2 {
		t.Fatalf("only %d IPMode constants found in internal/privacy/ip.go; the pattern "+
			"has stopped matching how they are written, so this test is comparing nothing",
			len(found))
	}

	// A real mode has to be probed, and at least one of its probes has
	// to expect acceptance. Both halves matter: a mode listed only in
	// its broken configuration would look covered while nothing ever
	// asserted that the two services can be started in it.
	probed, accepts := map[string]bool{}, map[string]bool{}
	for _, p := range ipModeProbes {
		probed[p.value] = true
		accepts[p.value] = accepts[p.value] || p.valid
	}
	for _, m := range found {
		mode := m[1]
		if !probed[mode] {
			t.Errorf("internal/privacy declares mode %q and no probe above mentions it.\n"+
				"Add it with a reason - a mode neither service is tested against is a "+
				"mode one of them can quietly not support", mode)
			continue
		}
		if !accepts[mode] {
			t.Errorf("%q is a real mode in internal/privacy and every probe for it "+
				"expects both services to refuse it, so nothing checks that it can be "+
				"configured at all", mode)
		}
	}
}

func accepted(ok bool) string {
	if ok {
		return "accepted"
	}
	return "refused"
}

// The two minimal files. Everything in them other than ip_storage is
// there because the loader requires it; nothing is decoration.
func loadBeaconWith(t *testing.T, dir, mode, key string) error {
	t.Helper()
	path := filepath.Join(dir, "beacon.toml")
	write(t, path, `
timescale_dsn = "postgres://beacon@127.0.0.1:5432/analytics"
sites = ["example-site"]

[privacy]
ip_storage = "`+mode+`"
ip_hash_key = "`+key+`"
`)
	_, err := beacon.LoadConfig(path)
	return err
}

func loadCollectorWith(t *testing.T, dir, mode, key string) error {
	t.Helper()
	path := filepath.Join(dir, "collector.toml")
	write(t, path, `
site_id = "example-site"

[network]
backend_addr = "127.0.0.1:8080"

[storage]
timescale_dsn = "postgres://collector@127.0.0.1:5432/analytics"

[privacy]
ip_storage = "`+mode+`"
ip_hash_key = "`+key+`"
`)
	_, err := collector.Load(path)
	return err
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
