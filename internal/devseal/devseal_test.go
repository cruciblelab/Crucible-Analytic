package devseal

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// cheap is what most tests here derive with.
//
// Current is 128 MiB and 378 ms, deliberately, and a test file that
// paid that thirty times would be a test file somebody stops running.
// The parameters travel inside the recipient, so a cheap one exercises
// exactly the same code as an expensive one - which is itself the
// property being relied on, and TestTheRealCostParametersWork checks it
// by paying once.
var cheap = Params{Memory: MinMemory, Time: MinTime, Threads: MinThreads}

func testIdentity(t *testing.T, password string) Identity {
	t.Helper()
	id, err := derive(password, cheap, bytes.Repeat([]byte{0x5a}, SaltLen))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return id
}

// The property the whole package exists for: what sealed a file cannot
// open it.
//
// There is no way to write this as "call Open on the sealing side and
// watch it fail", because the sealing side has no Open - Recipient has
// only Seal, and Open is on Identity, which cannot be reached without a
// password. That is a compiler guarantee rather than a test, and it is
// the stronger of the two.
//
// What is testable is the half a compiler cannot state: that the
// ephemeral private key is gone. If Seal kept it anywhere reachable,
// the box would be openable from the recipient alone. So this walks the
// only route back - deriving from the password - and checks that the
// route without it does not exist by checking that the ciphertext is
// different every time even for identical input, which is what a
// discarded ephemeral key produces.
func TestSealingTwiceProducesDifferentCiphertext(t *testing.T) {
	r := testIdentity(t, "correct horse battery staple").Recipient()

	firstEph, firstBox, err := r.Seal("header", []byte("the same plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	secondEph, secondBox, err := r.Seal("header", []byte("the same plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if firstEph == secondEph {
		t.Error("two seals used the same ephemeral key; it is not being drawn per seal")
	}
	if firstBox == secondBox {
		t.Error("two seals of the same plaintext produced the same ciphertext")
	}
}

func TestARoundTripReturnsExactlyWhatWentIn(t *testing.T) {
	const password = "correct horse battery staple"
	id := testIdentity(t, password)

	// Bytes rather than a string, and with a zero byte in them: this
	// carries a tar archive, and a path that went through a string
	// conversion badly would show up here rather than in production.
	plaintext := []byte("ip_hash_key = \"abc\"\x00\xff\n[privacy]\n")

	eph, box, err := id.Recipient().Seal("header", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Reopened from the encoded recipient rather than from id, because
	// that is what a restore does: it has a string out of a file and a
	// password, and nothing else.
	parsed, err := ParseRecipient(id.Recipient().String())
	if err != nil {
		t.Fatalf("parse recipient: %v", err)
	}
	back, err := Reopen(password, parsed)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := back.Open("header", eph, box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip changed the bytes:\n got %q\nwant %q", got, plaintext)
	}
}

func TestTheWrongPasswordIsRefusedBeforeAnythingIsDecrypted(t *testing.T) {
	id := testIdentity(t, "correct horse battery staple")

	_, err := Reopen("correct horse battery stapl", id.Recipient())
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("want ErrWrongPassword, got %v", err)
	}
}

func TestAChangedHeaderStopsTheBoxOpening(t *testing.T) {
	const password = "correct horse battery staple"
	id := testIdentity(t, password)

	eph, box, err := id.Recipient().Seal("taken 2026-01-01", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := id.Open("taken 2026-06-01", eph, box); !errors.Is(err, sealed.ErrCannotOpen) {
		t.Fatalf("a box opened under a header it was not sealed with: %v", err)
	}
}

func TestAChangedEphemeralKeyStopsTheBoxOpening(t *testing.T) {
	id := testIdentity(t, "correct horse battery staple")

	_, box, err := id.Recipient().Seal("header", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Another seal's ephemeral key: a well-formed X25519 point, so this
	// tests the binding rather than the parser.
	other, _, err := id.Recipient().Seal("header", []byte("other"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := id.Open("header", other, box); !errors.Is(err, sealed.ErrCannotOpen) {
		t.Fatalf("a box opened with the wrong ephemeral key: %v", err)
	}
}

func TestAChangedCiphertextStopsTheBoxOpening(t *testing.T) {
	id := testIdentity(t, "correct horse battery staple")

	eph, box, err := id.Recipient().Seal("header", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// One character of the base64 body, which is a change to the
	// ciphertext or the tag depending where it lands. Either must fail.
	flipped := []byte(box)
	flipped[len(flipped)-2] ^= 'A' ^ 'B'
	if _, err := id.Open("header", eph, string(flipped)); err == nil {
		t.Fatal("an edited ciphertext opened")
	}
}

func TestTheRecipientSurvivesItsEncoding(t *testing.T) {
	id := testIdentity(t, "correct horse battery staple")
	want := id.Recipient()

	got, err := ParseRecipient(want.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.String() != want.String() {
		t.Errorf("encoding round trip:\n got %s\nwant %s", got, want)
	}
	if got.Params() != want.Params() {
		t.Errorf("parameters did not survive: got %+v want %+v", got.Params(), want.Params())
	}
	// And the parsed one opens what the original sealed, which is the
	// part that matters: an encoding that round-trips its own text
	// while losing the key would pass the check above.
	eph, box, err := want.Seal("header", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	back, err := Reopen("correct horse battery staple", got)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := back.Open("header", eph, box); err != nil {
		t.Fatalf("the parsed recipient does not open what the original sealed: %v", err)
	}
}

func TestParseRefusesMalformedRecipients(t *testing.T) {
	good := testIdentity(t, "correct horse battery staple").Recipient().String()

	cases := []struct {
		name    string
		encoded string
		want    error
	}{
		{"empty", "", ErrNoRecipient},
		{"whitespace only", "   \n", ErrNoRecipient},
		{"no prefix", strings.TrimPrefix(good, prefix), ErrFormat},
		{"wrong prefix", "cadev2." + strings.TrimPrefix(good, prefix), ErrFormat},
		{"too few fields", prefix + "8192.1.1.aabb", ErrFormat},
		{"too many fields", good + ".extra", ErrFormat},
		{"memory not a number", prefix + "eight.1.1.00.00", ErrFormat},
		{"memory too large for the field", prefix + "68719476736.1.1.00.00", ErrFormat},
		{"time not a number", prefix + "8192.twice.1.00.00", ErrFormat},
		{"threads not a number", prefix + "8192.1.four.00.00", ErrFormat},
		{"salt not hex", prefix + "8192.1.1.zz.00", ErrFormat},
		{"salt too short", prefix + "8192.1.1.aabb.00", ErrFormat},
		{"key not a point", prefix + "8192.1.1." + strings.Repeat("aa", SaltLen) + ".00", ErrFormat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseRecipient(tc.encoded)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if r.IsSet() {
				t.Error("a refused recipient came back set")
			}
		})
	}
}

// The bound that protects the person restoring, not the ciphertext.
//
// The parameters travel in the file so that a restore works when the
// configuration is gone. That means an edited file can ask the opener
// to allocate whatever it names, and "m=64 GiB" is one line that stops
// a recovery dead. See the constants.
func TestParseRefusesCostParametersOutsideTheBounds(t *testing.T) {
	salt := strings.Repeat("aa", SaltLen)
	key := strings.TrimPrefix(
		testIdentity(t, "correct horse battery staple").Recipient().String(), prefix)
	key = key[strings.LastIndex(key, ".")+1:]

	cases := map[string]string{
		// 4 GiB, expressed in KiB as argon2 counts it. Chosen because
		// it fits in the field: a number so large it overflows uint32
		// is refused by the parser before the bound is consulted, which
		// is a different refusal and is covered above.
		"memory far above the maximum": "4194304.3.4",
		"memory just above":            "1048577.3.4",
		"memory below the minimum":     "1024.3.4",
		"time above the maximum":       "131072.17.4",
		"time zero":                    "131072.0.4",
		"threads above the maximum":    "131072.3.17",
		"threads zero":                 "131072.3.0",
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRecipient(prefix + params + "." + salt + "." + key)
			if !errors.Is(err, ErrParams) {
				t.Fatalf("got %v, want ErrParams", err)
			}
		})
	}

	// And the edges themselves are accepted, so the bound is a bound
	// and not an off-by-one that happens to refuse everything wrong.
	for name, params := range map[string]string{
		"at the minimum": "8192.1.1",
		"at the maximum": "1048576.16.16",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRecipient(prefix + params + "." + salt + "." + key); err != nil {
				t.Fatalf("the boundary value was refused: %v", err)
			}
		})
	}
}

func TestAnEmptyPasswordIsRefused(t *testing.T) {
	if _, err := Generate(""); err == nil {
		t.Error("Generate accepted an empty password")
	}
	id := testIdentity(t, "correct horse battery staple")
	if _, err := Reopen("", id.Recipient()); err == nil {
		t.Error("Reopen accepted an empty password")
	}
}

// A zero value must refuse rather than work with zeroes.
func TestTheZeroValuesRefuse(t *testing.T) {
	var r Recipient
	if r.IsSet() {
		t.Error("a zero Recipient reports itself as set")
	}
	if _, _, err := r.Seal("header", []byte("x")); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("a zero Recipient sealed something: %v", err)
	}
	if _, err := Reopen("password", r); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("Reopen accepted a zero Recipient: %v", err)
	}

	var i Identity
	if i.IsSet() {
		t.Error("a zero Identity reports itself as set")
	}
	if _, err := i.Open("header", "00", "v1.x"); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("a zero Identity opened something: %v", err)
	}
}

func TestOpenRefusesAMalformedEphemeralKey(t *testing.T) {
	id := testIdentity(t, "correct horse battery staple")
	for name, eph := range map[string]string{
		"not hex":    "zzzz",
		"too short":  "aabb",
		"empty":      "",
		"too long":   strings.Repeat("aa", KeyLen+1),
		"all zeroes": strings.Repeat("00", KeyLen),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := id.Open("header", eph, "v1.AAAA"); err == nil {
				t.Fatal("a malformed ephemeral key was accepted")
			}
		})
	}
}

// Two different passwords must not land on the same recipient, and the
// same password with a different salt must not either. Both would be
// silent: the file would seal and open and be openable by somebody it
// should not be.
func TestDifferentPasswordsAndSaltsGiveDifferentRecipients(t *testing.T) {
	salt := bytes.Repeat([]byte{0x5a}, SaltLen)
	other := bytes.Repeat([]byte{0x5b}, SaltLen)

	first, err := derive("correct horse battery staple", cheap, salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	second, err := derive("correct horse battery stapl", cheap, salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	third, err := derive("correct horse battery staple", cheap, other)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if first.Recipient().String() == second.Recipient().String() {
		t.Error("two passwords derived the same recipient")
	}
	if first.Recipient().String() == third.Recipient().String() {
		t.Error("the salt did not change the recipient")
	}
}

// The cost parameters must actually reach argon2.
//
// # Why this is not obvious enough to skip
//
// A derivation that ignored its Params and always used the cheapest
// allowed cost would pass every other test in this file. Round trips
// still work, because both sides ignore them the same way; the fixture
// still opens, because the fixture was made at the minimum. The only
// symptom is that every deployment's key is derived at 8 MiB and one
// pass instead of 128 MiB and three - which is the entire defence,
// silently gone.
//
// Measured, not imagined: a mutation replacing params.Time,
// params.Memory and params.Threads with the minimum survived the whole
// file until this test existed.
//
// Two different costs over the same password and salt must give two
// different keys. That is the cheapest statement that can only be true
// if the numbers are used.
func TestTheCostParametersReachTheDerivation(t *testing.T) {
	salt := bytes.Repeat([]byte{0x5a}, SaltLen)
	const password = "correct horse battery staple"

	low, err := derive(password, Params{Memory: MinMemory, Time: 1, Threads: 1}, salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for name, p := range map[string]Params{
		"more memory":  {Memory: 2 * MinMemory, Time: 1, Threads: 1},
		"more passes":  {Memory: MinMemory, Time: 2, Threads: 1},
		"more threads": {Memory: MinMemory, Time: 1, Threads: 2},
	} {
		t.Run(name, func(t *testing.T) {
			other, err := derive(password, p, salt)
			if err != nil {
				t.Fatalf("derive: %v", err)
			}
			if other.Recipient().pub.Equal(low.Recipient().pub) {
				t.Fatalf("%+v and %+v derived the same key, so the parameters are not "+
					"reaching argon2", p, low.Recipient().Params())
			}
		})
	}
}

// The same password and salt must derive the same key every time, or a
// backup sealed today does not open tomorrow.
func TestDerivationIsRepeatable(t *testing.T) {
	salt := bytes.Repeat([]byte{0x5a}, SaltLen)
	first, err := derive("correct horse battery staple", cheap, salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	second, err := derive("correct horse battery staple", cheap, salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if first.Recipient().String() != second.Recipient().String() {
		t.Error("the same password and salt derived two different recipients")
	}
}

func TestDeriveRefusesAWrongLengthSalt(t *testing.T) {
	for name, salt := range map[string][]byte{
		"empty":     {},
		"too short": bytes.Repeat([]byte{1}, SaltLen-1),
		"too long":  bytes.Repeat([]byte{1}, SaltLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := derive("correct horse battery staple", cheap, salt); err == nil {
				t.Fatal("a wrong-length salt was accepted")
			}
		})
	}
}

// The one test that pays the real cost, so that Current is exercised
// rather than only the cheap parameters every other test uses.
//
// Generate also draws a random salt, which nothing else here does: two
// calls with the same password must give different recipients, or every
// deployment would share one and a rainbow table would open all of
// them.
func TestTheRealCostParametersWork(t *testing.T) {
	const password = "correct horse battery staple"

	first, err := Generate(password)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The numbers, written out.
	//
	// The first version of this line compared against Current, which is
	// the variable it was checking - so lowering the cost passed. A
	// mutation caught it: m=64 MiB, t=2, p=2 was green.
	//
	// Written out here because this is a policy decision with a reason
	// attached (see Params), and lowering it is not a refactor. It is a
	// change to how expensive one offline guess is against every
	// secrets backup this product has ever produced, and it should be
	// made in front of a red test.
	if got := (first.Recipient().Params()); got != (Params{Memory: 128 * 1024, Time: 3, Threads: 4}) {
		t.Errorf("Generate used %+v. These parameters are the whole defence against "+
			"somebody guessing at a stolen backup offline; see Params for why they "+
			"are heavier than a login's", got)
	}

	second, err := Generate(password)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if first.Recipient().String() == second.Recipient().String() {
		t.Fatal("two deployments with the same password got the same recipient; " +
			"the salt is not random")
	}

	eph, box, err := first.Recipient().Seal("header", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	back, err := Reopen(password, first.Recipient())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := back.Open("header", eph, box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("got %q", got)
	}
	// And the other deployment's identity does not open it, which is
	// the salt doing its job rather than the password.
	otherBack, err := Reopen(password, second.Recipient())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := otherBack.Open("header", eph, box); err == nil {
		t.Fatal("a different deployment's identity opened this file")
	}
}

// The format, pinned.
//
// # What a round trip cannot tell you
//
// Every other test here seals and opens with the same build, so every
// one of them passes after a change that alters both sides together:
// the domain separator, the order of the keys in the HKDF salt, the
// hash, the AEAD. Measured rather than assumed - both mutations survive
// the whole file above.
//
// And that class of change is the one with the worst failure. Nothing
// goes red, nothing looks wrong, and every secrets backup taken before
// the change stops opening. Nobody finds out until the day somebody
// needs one, which is the day there is no second chance.
//
// So this is a file sealed by an earlier build, written down, opened by
// this one. It is not a value to regenerate when it fails. A failure
// here means today's build cannot read yesterday's backups, and the
// question it asks is whether that is intended - if it is, the format
// version in `domain` and in `prefix` changes with it, and a new fixture
// is added *beside* this one rather than over it.
const (
	fixtureRecipient = "cadev1.8192.1.1.11111111111111111111111111111111." +
		"643cba3640428a8caae3c10c96cc591e13c05a5f3f102d40393237339c401334"
	fixtureEphemeral = "206d184d07f5c627109c43ac89772b03fdf9b30df88c23185612db8652ae4c49"
	fixtureBox       = "v1.euE+7O7u5pkCxsf887cx99tBDMlS7aWUQ/HQSsjtzPPqqeRz1V8IdWQ="
	fixtureHeader    = "crucible-analytic/secrets-backup fixture v1"
	fixturePassword  = "sabit-fikstur-parolasi-uzun"
	fixturePlaintext = "sabit içerik"
)

func TestAFileSealedByAnEarlierBuildStillOpens(t *testing.T) {
	r, err := ParseRecipient(fixtureRecipient)
	if err != nil {
		t.Fatalf("the recorded recipient no longer parses: %v", err)
	}
	id, err := Reopen(fixturePassword, r)
	if err != nil {
		t.Fatalf("the recorded password no longer derives the recorded recipient: %v.\n"+
			"Something about the key derivation changed - the argon2 parameters, the "+
			"seed-to-scalar step, or how the salt is read", err)
	}
	got, err := id.Open(fixtureHeader, fixtureEphemeral, fixtureBox)
	if err != nil {
		t.Fatalf("a file sealed by an earlier build does not open: %v.\n"+
			"Every secrets backup taken before this change is now unreadable. If that "+
			"is intended, bump the version in domain and in prefix and add a new "+
			"fixture beside this one", err)
	}
	if string(got) != fixturePlaintext {
		t.Errorf("got %q, want %q", got, fixturePlaintext)
	}
}
