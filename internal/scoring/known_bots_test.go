package scoring

import "testing"

// TestNilSetIsUsable is the property the whole licence decision rests
// on: a deployment that has never fetched the dataset has no set, and
// nothing anywhere may need to check for that.
func TestNilSetIsUsable(t *testing.T) {
	var none KnownBots
	if none.Known("t13d1516h2_8daaf6152771_b186095e22b6") {
		t.Error("the nil set claimed to know a fingerprint")
	}
	if label, ok := none.Label("anything"); ok || label != "" {
		t.Errorf("the nil set returned %q, %v", label, ok)
	}
	if none.Len() != 0 {
		t.Error("the nil set has a length")
	}
}

// TestEmptyFingerprintIsNeverKnown. "" is the sentinel every caller in
// this project uses for "no JA4 was available" - non-TLS traffic, an
// unparseable ClientHello. Treating it as a lookup key would label all
// of that as a known bot, so it is refused before the map is consulted
// and even when the map contains it.
func TestEmptyFingerprintIsNeverKnown(t *testing.T) {
	poisoned := KnownBots{"": "would flag everything"}
	if poisoned.Known("") {
		t.Fatal("an empty fingerprint matched; all non-TLS traffic would be called a bot")
	}
	if label, ok := poisoned.Label(""); ok || label != "" {
		t.Fatalf("Label(\"\") = %q, %v", label, ok)
	}
}

func TestLookup(t *testing.T) {
	set := KnownBots{"t13d1516h2_8daaf6152771_b186095e22b6": "curl"}
	label, ok := set.Label("t13d1516h2_8daaf6152771_b186095e22b6")
	if !ok || label != "curl" {
		t.Errorf("Label = %q, %v", label, ok)
	}
	if set.Known("t13d1516h2_0000000000_000000000000") {
		t.Error("an unknown fingerprint matched")
	}
	if set.Len() != 1 {
		t.Errorf("Len = %d", set.Len())
	}
}
