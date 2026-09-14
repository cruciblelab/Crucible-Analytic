package privacy

import (
	"strconv"
	"time"
)

// The answer to "what is collected about me here", derived rather than
// written.
//
// # Why this is a struct of facts and not a paragraph
//
// Two readers want two different things and only one of them reads
// prose. A customer printing this on their own privacy page needs it in
// their own language and their own design - so the JSON endpoint carries
// facts, and what to say about them is theirs. The ready-made page needs
// sentences, and it builds them from these same facts.
//
// One source, two encodings. A paragraph here would have to be
// translated by whoever embeds it, which means retyped, which means a
// second copy that stops agreeing with the mode the moment somebody
// changes it.
//
// # Why it takes the mode rather than reading a setting
//
// Because the caller has the live one. The beacon reads
// `ip_storage` through an atomic that SetIPMode swaps while the process
// runs, so a mode changed on the panel reaches the next request without
// a restart. A Notice that read a config file instead would be right on
// the day it was deployed and wrong afterwards, which is the failure
// this whole shape exists to avoid.
type Notice struct {
	// IPStorage is the mode in force, as its id.
	IPStorage IPMode `json:"ip_storage"`

	// AddressKept says what is written for the visitor's address.
	//
	// Both modes store the masked address; full mode adds a keyed
	// token derived from the whole one. Neither writes the address
	// itself, and that is the sentence worth being precise about: "we
	// do not store your IP" is true in both, and it is true for
	// different reasons.
	AddressMaskedTo string `json:"address_masked_to"`

	// TokenFromWholeAddress is whether a keyed token derived from the
	// complete address is stored alongside the masked one.
	//
	// This is the whole behavioural difference between the modes, so it
	// is a field rather than something a reader has to infer from the
	// mode's name. False in masked mode, true in full.
	TokenFromWholeAddress bool `json:"token_from_whole_address"`

	// TellsVisitorsApartWithinANetwork is what that token buys.
	//
	// In masked mode everybody behind one /24 is one row's worth of
	// address, so two people in the same office cannot be told apart.
	// In full mode they can. Stated as a capability rather than as a
	// mechanism because that is the part a visitor actually cares
	// about.
	TellsVisitorsApartWithinANetwork bool `json:"tells_visitors_apart_within_a_network"`

	// Cookies is whether any cookie is set. It is not, in either mode.
	Cookies bool `json:"cookies"`

	// IdentifierRotatesEvery is how long a visitor identifier lasts.
	//
	// The identifier is derived with a secret that is generated at
	// startup, held only in memory, and replaced on this period - see
	// internal/beacon.VisitorIDs. Once it is replaced, an old
	// identifier cannot be re-derived from an address by anybody,
	// including whoever holds the database.
	IdentifierRotatesEvery time.Duration `json:"identifier_rotates_every_seconds"`

	// DeletionOnRequest is whether a visitor can ask for their records
	// to be deleted. They cannot, and the reason is the field below.
	DeletionOnRequest bool `json:"deletion_on_request"`

	// NoDeletionReason names why, as an id rather than a sentence.
	//
	// An id so the ready-made page and a customer's own page say the
	// same thing in two languages without either of them being a
	// translation of the other.
	//
	// The reason is not a policy: after the day's secret is replaced,
	// nothing can point at one visitor's rows - so there is no set to
	// hand over and none to delete. Within the current period it would
	// take the visitor's own address and browser string, handed over
	// deliberately, which is collecting more to delete less.
	NoDeletionReason string `json:"no_deletion_reason"`

	// OptOut is the call a visitor makes to stop being counted, from
	// P1. Named here so the disclosure and the script cannot drift.
	OptOut string `json:"opt_out"`

	// PolicyURL is the operator's own privacy page, when they have set
	// one. Empty otherwise, and empty is the ordinary state.
	//
	// The one field here that is not derived from anything: it is a fact
	// about the deployment that only the operator knows. It is carried
	// rather than assumed, and it is checked before it is shown - see
	// CleanPolicyURL for what a page served to the public must not
	// contain.
	PolicyURL string `json:"policy_url,omitempty"`

	// Contact is where a visitor's request should go when the operator
	// handles those by hand. An address or a page; empty when unset.
	Contact string `json:"contact,omitempty"`

	// EffectiveSince is when the mode above was last written, when that
	// is known.
	//
	// # Why a disclosure needs a date
	//
	// Every other field here says what is collected *now*, and a page
	// built from them is correct the moment it is served. What it cannot
	// say is that it is new. privacy.ip_storage moving from masked to
	// full means more personal data from the next request on, and this
	// notice changes underneath a visitor who read it yesterday with
	// nothing marking the change.
	//
	// The date is what makes that detectable at both ends: a visitor
	// reads it on the page, and a customer's own page - which consumes
	// the JSON rather than the prose - can compare it against the one it
	// last saw and, having both, compute *which way* the mode moved from
	// the fields beside it.
	//
	// # Why not the direction itself
	//
	// Because the previous value is in the audit log and the beacon must
	// not be able to read the panel's tables. Saying "escalated from
	// masked" here would mean either crossing that boundary or keeping a
	// second copy of the history where the beacon can see it - and a
	// second copy of a history is a history that can disagree with
	// itself. The date crosses nothing: panel_settings.updated_at is in
	// the same row as the value, which the service already reads.
	//
	// Zero when unknown, and unknown is an ordinary state: a deployment
	// that never changed the setting from the panel has no row, so the
	// mode comes from the service's config file and this table never
	// carried a date for it. Reporting the install time or the process
	// start instead would be a date about something else.
	//
	// # omitzero, not omitempty
	//
	// omitempty does nothing to a struct field, and time.Time is a
	// struct: written that way the key is always present, carrying
	// 0001-01-01T00:00:00Z on every deployment that never set the mode
	// from the panel. A consumer comparing that against the date it
	// last saw would read a change where there was none, and the only
	// reader of this field is exactly such a consumer. omitzero (Go
	// 1.24) consults IsZero and leaves the key out.
	EffectiveSince time.Time `json:"effective_since,omitzero"`
}

// NoDeletionNoIdentity is the reason id.
//
// One constant rather than a string typed twice: the page renders a
// sentence for it and a test asserts the page renders one for whatever
// NewNotice returns, so an id added later cannot reach a visitor as a
// blank.
const NoDeletionNoIdentity = "no_durable_identity"

// OptOutCall is what a visitor runs in their browser's console, and what
// a consent banner calls.
//
// Here rather than in the disclosure's prose because it is an API name:
// beacon.js defines window.crucible.optOut, and a disclosure that named
// it differently would be instructions that do nothing.
const OptOutCall = "window.crucible.optOut()"

// NewNotice derives the disclosure from the mode in force.
//
// Everything in it is a consequence of the mode or of a decision written
// down elsewhere in this package. Nothing is a constant somebody chose
// while writing a privacy page.
func NewNotice(mode IPMode, rotates time.Duration) Notice {
	mode = ParseIPMode(string(mode))
	return Notice{
		IPStorage:                        mode,
		AddressMaskedTo:                  maskedTo(),
		TokenFromWholeAddress:            mode.Tokenises(),
		TellsVisitorsApartWithinANetwork: mode.Tokenises(),
		Cookies:                          false,
		IdentifierRotatesEvery:           rotates,
		DeletionOnRequest:                false,
		NoDeletionReason:                 NoDeletionNoIdentity,
		OptOut:                           OptOutCall,
	}
}

// maskedTo describes the prefix lengths kept, from the constants that
// actually do the masking.
//
// Derived from maskedIPv4Bits and maskedIPv6Bits rather than written as
// "/24 and /64": those two numbers are a decision recorded in this file
// with a paragraph of reasoning, and a disclosure that repeated them by
// hand would be a second copy that keeps saying /24 the day somebody
// changes it.
//
// It takes no mode, and that is a fact rather than an omission: both
// modes mask to the same lengths. Full mode adds a token; it does not
// keep more of the address.
func maskedTo() string {
	return "IPv4 /" + strconv.Itoa(maskedIPv4Bits) + ", IPv6 /" + strconv.Itoa(maskedIPv6Bits)
}
