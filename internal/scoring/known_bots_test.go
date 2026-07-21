package scoring

import "testing"

func TestKnownBotJA4_LoadsRealSnapshotData(t *testing.T) {
	// Pins the current known_bots.json snapshot (The Bot Aquarium archive,
	// browser-classified entries excluded, retrieved 2026-07-21). Update
	// this count deliberately if the snapshot is refreshed.
	const wantCount = 51
	if len(KnownBotJA4) != wantCount {
		t.Errorf("len(KnownBotJA4) = %d, want %d", len(KnownBotJA4), wantCount)
	}

	// Spot-check one real, known entry rather than only asserting a count.
	const curlJA4 = "t13d4907h2_0d8feac7bc37_7395dae3b2f3"
	label, ok := KnownBotJA4[curlJA4]
	if !ok {
		t.Fatalf("KnownBotJA4 missing expected entry %s", curlJA4)
	}
	if label != "curl" {
		t.Errorf("KnownBotJA4[%s] = %q, want %q", curlJA4, label, "curl")
	}
}

func TestKnownBotJA4_ExcludesBrowserEntries(t *testing.T) {
	// The source archive has ~46 entries classified "browser" (legitimate
	// reference fingerprints, not bot signal); none of their labels should
	// have made it into the loaded map.
	for ja4, label := range KnownBotJA4 {
		if label == "browser" {
			t.Errorf("KnownBotJA4[%s] = %q: browser-classified entries must be excluded", ja4, label)
		}
	}
}

func TestMustLoadKnownBots_DropsEmptyJA4Key(t *testing.T) {
	orig := knownBotsJSON
	defer func() { knownBotsJSON = orig }()

	knownBotsJSON = []byte(`{"entries":[
		{"ja4":"", "label":"should-be-dropped"},
		{"ja4":"real-ja4", "label":"kept"}
	]}`)

	got := mustLoadKnownBots()
	if _, ok := got[""]; ok {
		t.Error("mustLoadKnownBots() kept an empty-string JA4 key, want it dropped")
	}
	if got["real-ja4"] != "kept" {
		t.Errorf("mustLoadKnownBots() = %v, want real-ja4 entry preserved", got)
	}
}

func TestMustLoadKnownBots_PanicsOnMalformedJSON(t *testing.T) {
	orig := knownBotsJSON
	defer func() { knownBotsJSON = orig }()
	knownBotsJSON = []byte(`{not valid json`)

	defer func() {
		if recover() == nil {
			t.Error("mustLoadKnownBots() did not panic on malformed JSON")
		}
	}()
	mustLoadKnownBots()
}
