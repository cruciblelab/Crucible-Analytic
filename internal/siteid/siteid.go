// Package siteid is what a site identifier may be, in one place.
//
// # Why one place
//
// The collector and the beacon each carried the rule, with a comment
// saying the two must stay identical and nothing holding them so - and
// the panel carried none at all. Its `beacon.sites` setting took any
// string, the beacon applied the list without asking, and an event naming
// such a site was written under it (PLAN §H8).
//
// # What the rule protects
//
//   - Joins. The collector writes traffic_snapshots under its site_id and
//     the beacon writes beacon_events under the event's; a beacon that
//     accepted an identifier the collector could never have written would
//     produce rows nothing can join to.
//   - Routes and file names. The read API's routes and the backup's file
//     names use the identifier verbatim, with no encoding step that could
//     disagree between the writer and the reader.
//   - Index entries. site_id is a key in every index this product keeps
//     per site, and PostgreSQL refuses an entry over 2704 bytes along with
//     the statement that wrote it - for the beacon, the whole batch's COPY
//     (PLAN §H7). Sixty-four ASCII characters are sixty-four bytes.
//
// # Two questions, and why they are not one
//
// Valid is what a site identifier is: the pattern the collector and the
// beacon hold their configuration files to. Keyable is what can be stored
// where a site identifier is a key: short enough for every index, and
// nothing a text column refuses.
//
// The panel's site list and the panel's own pages ask the second, not the
// first, because they read values that may predate any rule. The panel's
// list took any string until H8; a deployment that put "www.ornek.com" in
// it has had a beacon recording that site ever since, and the panel
// re-validates every stored setting when it reads it - so holding the
// list to the pattern would turn one dotted entry into an empty list, and
// an upgrade into a site nobody is recording. Nothing about a dot hurts
// an index. What does is length, and a character PostgreSQL refuses.
package siteid

import (
	"regexp"

	"github.com/cruciblelab/crucible-analytic/internal/textsafe"
)

// MaxLen is the longest identifier, in characters and so in bytes: the
// pattern admits ASCII only.
const MaxLen = 64

var pattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Valid reports whether s is a site identifier this product accepts:
// 1 to MaxLen letters, digits, underscores or dashes.
func Valid(s string) bool { return pattern.MatchString(s) }

// MaxKeyBytes is the longest site identifier stored as a key, in bytes.
//
// The tightest index it sits in is the beacon's campaign index, beside
// three campaign values of up to 512 bytes each and a timestamp; 256
// leaves that entry near 1.9 KB, under PostgreSQL's 2704. Four times the
// pattern's bound, so no identifier any configuration file accepts comes
// near it.
const MaxKeyBytes = 256

// Keyable reports whether s can be stored where a site identifier is a
// key: not empty, at most MaxKeyBytes, and nothing a text column refuses.
// Every Valid identifier is Keyable.
func Keyable(s string) bool {
	return s != "" && len(s) <= MaxKeyBytes && textsafe.Storable(s) == s
}
