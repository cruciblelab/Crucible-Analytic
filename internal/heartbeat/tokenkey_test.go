package heartbeat

import (
	"bytes"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// TestTheOptionalColumnsAreOneList is the test optionalColumns' comment
// says holds the writer and the reader together.
//
// It said so before it existed, which is the reason to write it rather
// than to trust it: a comment claiming a test is a claim nothing checks.
//
// # What getting it wrong would look like
//
// profile and ip_token_key_state are both text, so PostgreSQL accepts a
// row with the two values swapped without a word. The health page would
// then show a profile named "present" and the panel would read a
// token-key state of "dengeli" - which ParseTokenKeyState turns into
// unknown, which refuses full mode. A deployment with a key on disk in
// both services, refused, for a reason visible nowhere.
func TestTheOptionalColumnsAreOneList(t *testing.T) {
	// A reporter whose two optional values are distinguishable, so the
	// pairing is checked and not only the order. Written as the values a
	// real collector would carry.
	r := &Reporter{profile: "dengeli", ipTokenKey: TokenKeyPresent}

	var order []string
	got := map[string]any{}
	for _, opt := range r.reported() {
		order = append(order, opt.column)
		got[opt.column] = opt.value
	}

	if !slices.Equal(order, optionalColumns) {
		t.Errorf("reported() names %v, optionalColumns says %v.\n"+
			"The writer pairs values by this order and the reader scans by it, so the "+
			"two lists have to be one list in one order.", order, optionalColumns)
	}

	// And each column carries its own field's value. A swap here passes
	// the order check above, because the order is still right - it is the
	// pairing that is wrong.
	for column, want := range map[string]any{
		"profile":            "dengeli",
		"ip_token_key_state": string(TokenKeyPresent),
	} {
		if got[column] != want {
			t.Errorf("%s would be written as %v, want %v: this value belongs to another column",
				column, got[column], want)
		}
	}
}

// TestReportingAKeyMeansATokenComesOut.
//
// # The claim, and why it is not "the string is not empty"
//
// A service reporting present is telling the panel it could tokenise an
// address, and the panel opens full mode on that sentence. So the
// threshold has to be the one TokenIP applies at the moment of use, not
// a laxer one that happens to read the same way: a reporter saying yes
// by a lower bar would let full mode be selected on a deployment that
// then writes the masked address and no token at all - silently in
// masked mode while its setting says otherwise, which is the failure the
// precondition exists to prevent.
//
// So this is written as an agreement rather than as a number. The bound
// is privacy.MinHashKeyLen, taken from that package rather than repeated
// here, and the assertion is that the state and the token agree for
// every length around it. A change to the constant cannot leave the two
// sides disagreeing, and nothing in this test needs editing when it
// moves.
func TestReportingAKeyMeansATokenComesOut(t *testing.T) {
	visitor := netip.MustParseAddr("203.0.113.9")

	for _, n := range []int{0, 1, 16, privacy.MinHashKeyLen - 1, privacy.MinHashKeyLen,
		privacy.MinHashKeyLen + 1, 64} {
		key := bytes.Repeat([]byte{0x7}, n)
		state := TokenKeyStateOf(key)
		token := privacy.TokenIP(visitor, key)

		if (state == TokenKeyPresent) != (len(token) > 0) {
			t.Errorf("a %d-byte key reports %q while TokenIP returns %d bytes.\n"+
				"present has to mean a token actually comes out; anything else offers "+
				"full mode to a deployment that would write no tokens.", n, state, len(token))
		}
		if state != TokenKeyPresent && state != TokenKeyAbsent {
			t.Errorf("a %d-byte key reports %q; a service that looked reports one of the "+
				"two answers, never unknown - unknown is for a build that cannot answer",
				n, state)
		}
	}

	// A key nobody configured is absent rather than unknown, for the same
	// reason: this service looked.
	if got := TokenKeyStateOf(nil); got != TokenKeyAbsent {
		t.Errorf("TokenKeyStateOf(nil) = %q, want %q", got, TokenKeyAbsent)
	}

	// And the bound is MinHashKeyLen, named here rather than taken from
	// CanTokenise.
	//
	// The loop above cannot see that function move, and a mutation showed
	// it: replacing CanTokenise's body with len(key) > 0 keeps every
	// assertion green, because TokenKeyStateOf and TokenIP both ask the
	// same function and so still agree. A test that takes both sides of
	// an agreement from one place is testing that they agree, not what
	// they agree on.
	//
	// So the threshold is asserted against the constant that documents
	// it - and MinHashKeyLen's own comment says why 32: a short key is
	// what makes brute force easy in the direction that matters, which is
	// guessing the key from an address-and-hash pair anybody who can
	// visit the site can produce for themselves.
	if got := TokenKeyStateOf(bytes.Repeat([]byte{0x7}, privacy.MinHashKeyLen-1)); got != TokenKeyAbsent {
		t.Errorf("a key one byte under privacy.MinHashKeyLen reports %q.\n"+
			"A key too short to use is a key this service does not have; reporting it as "+
			"present offers full mode to a deployment whose tokens reverse in "+
			"microseconds.", got)
	}
	if got := TokenKeyStateOf(bytes.Repeat([]byte{0x7}, privacy.MinHashKeyLen)); got != TokenKeyPresent {
		t.Errorf("a key of exactly privacy.MinHashKeyLen reports %q, want present: the bound "+
			"is inclusive, and a service holding the documented minimum holds a usable "+
			"key", got)
	}
}

// TestAnUnrecognisedStateRefuses.
//
// The permissive direction is the safe one here, which is unusual enough
// to be worth a test: anything the reader does not recognise becomes
// unknown, and unknown refuses full mode. So a future writer inventing a
// fourth word can cost a deployment the fast answer and can never open
// the gate by accident.
func TestAnUnrecognisedStateRefuses(t *testing.T) {
	for _, value := range []string{"", "PRESENT", "present ", "yes", "true", "1",
		"dengeli", "absent-ish", "hazır"} {
		if got := ParseTokenKeyState(value); got != TokenKeyUnknown {
			t.Errorf("ParseTokenKeyState(%q) = %q, want unknown", value, got)
		}
	}
	// And the two it does recognise, so the test above is not passing by
	// recognising nothing at all.
	if got := ParseTokenKeyState(string(TokenKeyPresent)); got != TokenKeyPresent {
		t.Errorf("ParseTokenKeyState(%q) = %q", TokenKeyPresent, got)
	}
	if got := ParseTokenKeyState(string(TokenKeyAbsent)); got != TokenKeyAbsent {
		t.Errorf("ParseTokenKeyState(%q) = %q", TokenKeyAbsent, got)
	}
}

// TestEveryOptionalColumnIsInTheSchemaFile.
//
// # The mutation that survived without it
//
// Deleting the ADD COLUMN line for ip_token_key_state from schema.sql
// broke nothing. Every suite that reads the column runs against a
// database where it already exists - applied by an earlier run, or by
// the installer - so no test applies that file to a database lacking it
// and then asks.
//
// The product would have shipped a binary that reports the state and a
// schema that never creates the column. What happens then is the
// accommodation working exactly as designed and exactly wrongly: the
// reporter detects the column is absent, drops it from its write, the
// panel reads unreported for every service, and full mode is refused on
// every deployment. Which is the defect 5b exists to remove, arriving
// from the other end.
//
// # Why this is a mirror and not a query
//
// A database check would need a fresh database and would only answer
// for the moment it ran. The claim is about the source: every optional
// column the writer knows about is a column the schema file creates.
// Both sides are derived - the Go list and the file's own text - so a
// third optional column added tomorrow is held to the rule without
// anybody coming back here.
//
// ADD COLUMN IF NOT EXISTS specifically, rather than the name appearing
// anywhere in the file. The name appears in this file's prose several
// times, at length; a scan that accepted a mention would pass on a
// comment describing the column somebody forgot to add.
func TestEveryOptionalColumnIsInTheSchemaFile(t *testing.T) {
	source, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(source)

	for _, column := range optionalColumns {
		want := "ADD COLUMN IF NOT EXISTS " + column + " "
		if !strings.Contains(sql, want) {
			t.Errorf(`internal/heartbeat/schema.sql has no %q.

The writer reports this column and the schema never creates it. On a
fresh installation the reporter would find it missing, drop it from
every write, and the panel would read "not reported" for every service -
refusing full IP mode on every deployment, which is the defect this
column was added to remove.`, want)
		}
	}

	// And nothing the schema creates is missing from the Go list, which
	// is the direction that would leave a column written by nobody.
	// Derived from the file rather than listed here.
	adds := regexp.MustCompile(`ADD COLUMN IF NOT EXISTS ([a-z_]+) `).FindAllStringSubmatch(sql, -1)
	if len(adds) == 0 {
		t.Fatal("no ADD COLUMN statements found in schema.sql; this test is reading nothing")
	}
	for _, m := range adds {
		if !slices.Contains(optionalColumns, m[1]) {
			t.Errorf("schema.sql adds %q and optionalColumns does not name it, so no writer "+
				"ever fills it and no reader ever selects it", m[1])
		}
	}
}
