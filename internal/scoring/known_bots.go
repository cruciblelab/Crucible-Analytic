package scoring

// KnownBots is a set of JA4 fingerprints belonging to automation tools,
// mapping each to a human-readable label.
//
// It is a value the caller supplies rather than data compiled into this
// package, and that is a licence decision as much as a design one. The
// dataset belongs to somebody else; this repository is MIT and does not
// redistribute it. A deployment fetches it onto its own machine with
// `collector -update-bot-data` - see internal/botdata.
//
// The nil set is meaningful and supported: a deployment that has never
// fetched has no known-bot signal, every other signal still works, and
// nothing here pretends otherwise. Lookup handles nil, so no caller
// needs to check.
type KnownBots map[string]string

// Label returns the label for a fingerprint, and whether it is known.
//
// An empty fingerprint is never known, whatever the set contains. ""
// is the sentinel every caller in this project uses for "no JA4 was
// available" - non-TLS traffic, an unparseable ClientHello - and
// treating it as a lookup key would label all of that as a known bot.
func (k KnownBots) Label(ja4 string) (string, bool) {
	if ja4 == "" {
		return "", false
	}
	label, ok := k[ja4]
	return label, ok
}

// Known reports whether the fingerprint belongs to a known bot.
func (k KnownBots) Known(ja4 string) bool {
	_, ok := k.Label(ja4)
	return ok
}

// Len reports how many fingerprints are in the set.
func (k KnownBots) Len() int { return len(k) }
