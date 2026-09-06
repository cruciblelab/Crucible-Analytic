package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestStorableRemovesExactlyWhatCannotBeWritten.
func TestStorableRemovesExactlyWhatCannotBeWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		why  string
	}{
		{
			name: "bos", in: "", want: "",
			why: "the empty string is already storable",
		},
		{
			name: "duz", in: "Türk Telekom", want: "Türk Telekom",
			why: "multi-byte runes are not control characters and must survive; " +
				"a pass that stripped them would empty half this product's " +
				"Turkish text",
		},
		{
			name: "nul", in: "a\x00b", want: "ab",
			why: "the one byte PostgreSQL refuses outright",
		},
		{
			name: "gecersiz", in: "a\xffb", want: "ab",
			why: "invalid UTF-8 is refused by the column the same way a NUL is",
		},
		{
			name: "satirbasi", in: "a\nb\rc", want: "abc",
			why: "a newline in a logged value splits one JSON record into two, " +
				"and the second half is chosen by whoever sent the request",
		},
		{
			name: "sekme", in: "a\tb", want: "ab",
			why: "tab is in the control range; it breaks column-oriented output",
		},
		{
			name: "del", in: "a\x7fb", want: "ab",
			why: "DEL is above the control range and is still a control character",
		},
		{
			name: "unicode ayirici", in: "a b c", want: "abc",
			why: "the Unicode line and paragraph separators. Valid UTF-8, not " +
				"ASCII, and treated as line breaks by some JSON readers - so " +
				"they are the case a check written as 'r < 0x20' misses",
		},
		{
			name: "bosluk korunur", in: " a b ", want: " a b ",
			why: "trimming is the caller's policy, not this rule. beacon trims " +
				"and logging does not, and folding one into the other here " +
				"would change one of them silently",
		},
		{
			name: "emoji", in: "bot \U0001F916", want: "bot \U0001F916",
			why: "a four-byte rune is not a control character either",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Storable(tc.in)
			if got != tc.want {
				t.Errorf("Storable(%q) = %q, want %q.\n%s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// FuzzStorableAlwaysReturnsSomethingAColumnAccepts.
//
// The table above checks the cases somebody thought of. This checks the
// property itself, which is the only reason the function exists: whatever
// goes in, what comes out can be written to a PostgreSQL TEXT column and
// can be one line of a log file.
//
// Idempotence is checked alongside it. Not for its own sake: a pass that
// changes its answer on a second application is one whose output is not
// actually in the set it claims to produce, and that is the same defect
// wearing different clothes.
func FuzzStorableAlwaysReturnsSomethingAColumnAccepts(f *testing.F) {
	f.Add("Türk Telekom")
	f.Add("a\x00b")
	f.Add("a\xffb")
	f.Add("  ")
	f.Add(strings.Repeat("\x01", 64))
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		got := Storable(s)

		if !utf8.ValidString(got) {
			t.Fatalf("Storable(%q) = %q, which is not valid UTF-8", s, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
				t.Fatalf("Storable(%q) kept %q", s, r)
			}
		}
		if again := Storable(got); again != got {
			t.Fatalf("Storable is not idempotent: %q became %q and then %q",
				s, got, again)
		}
		// It removes and never adds. A pass that substituted a
		// replacement character would grow the string, and a caller that
		// bounds the result afterwards would then be bounding something
		// this function chose rather than something the source sent.
		if utf8.RuneCountInString(got) > utf8.RuneCountInString(s) {
			t.Fatalf("Storable(%q) = %q, which is longer in runes", s, got)
		}
	})
}
