package scoring

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed known_bots.json
var knownBotsJSON []byte

// knownBotsFile mirrors known_bots.json's structure. See that file's
// "source"/"retrieved_at"/"note" fields for exactly where this data came
// from and its known limitations.
type knownBotsFile struct {
	Source      string          `json:"source"`
	SourceAPI   string          `json:"source_api"`
	RetrievedAt string          `json:"retrieved_at"`
	Note        string          `json:"note"`
	Entries     []knownBotEntry `json:"entries"`
}

type knownBotEntry struct {
	JA4              string   `json:"ja4"`
	BotTypes         []string `json:"bot_types"`
	Label            string   `json:"label"`
	ExampleUserAgent string   `json:"example_user_agent"`
	SubmissionCount  int      `json:"submission_count"`
}

// KnownBotJA4 maps JA4 fingerprints to a human-readable label, loaded from
// known_bots.json at build time via go:embed - a real (if necessarily
// partial and aging) dataset of automation-tool/bot fingerprints from The
// Bot Aquarium's public archive, not hardcoded placeholder data. See
// known_bots.json's own "note" field and the project README for its
// sourcing, exclusions (browser-classified entries are dropped - see
// mustLoadKnownBots), and update-cadence caveats: this is a one-time
// snapshot with no automatic refresh mechanism in this MVP.
var KnownBotJA4 = mustLoadKnownBots()

func mustLoadKnownBots() map[string]string {
	var f knownBotsFile
	if err := json.Unmarshal(knownBotsJSON, &f); err != nil {
		panic(fmt.Sprintf("scoring: invalid known_bots.json: %v", err))
	}

	m := make(map[string]string, len(f.Entries))
	for _, e := range f.Entries {
		if e.JA4 == "" {
			// Defense in depth: "" is the sentinel Score() and this map's
			// callers use for "no JA4 available" (see Score's doc comment).
			// An empty-string entry here would flag all non-TLS/unparseable
			// traffic as a known bot, so it's dropped rather than trusted
			// even though the source data isn't expected to contain one.
			continue
		}
		m[e.JA4] = e.Label
	}
	return m
}
