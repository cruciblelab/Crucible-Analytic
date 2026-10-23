package siteid

import (
	"strings"
	"testing"
)

// The rule at its edges. The bound is written out rather than taken from
// MaxLen, so a change to the constant has to change this line too.
func TestTheRuleAtItsEdges(t *testing.T) {
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"magaza", true},
		{"Magaza_2-tr", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"bir site", false},
		{"mağaza", false},
		{"site/../x", false},
		{"site\x00", false},
		{"site\n", false},
	} {
		if got := Valid(c.id); got != c.want {
			t.Errorf("Valid(%.20q) = %v, want %v", c.id, got, c.want)
		}
	}
	if MaxLen != 64 {
		t.Errorf("MaxLen = %d; the pattern and the cases above say 64", MaxLen)
	}
}

// Keyable at its edges, and the promise that ties the two questions: no
// identifier the pattern accepts is refused as a key. The bound is
// written out, for the same reason as above.
func TestKeyableAtItsEdges(t *testing.T) {
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"magaza", true},
		{"www.ornek.com", true},
		{"bir site", true},
		{"mağaza", true},
		{strings.Repeat("x", 256), true},
		{strings.Repeat("x", 257), false},
		{strings.Repeat("ğ", 128), true},
		{strings.Repeat("ğ", 129), false},
		{"", false},
		{"site\x00", false},
		{"site\x01", false},
		{"bozuk\xff", false},
	} {
		if got := Keyable(c.id); got != c.want {
			t.Errorf("Keyable(%.20q) = %v, want %v", c.id, got, c.want)
		}
	}
	for _, id := range []string{"a", strings.Repeat("Z", MaxLen), "a_b-c9"} {
		if Valid(id) && !Keyable(id) {
			t.Errorf("%q is a valid site identifier and not keyable", id)
		}
	}
}
