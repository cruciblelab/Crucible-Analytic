package web

import (
	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The "About" block at the foot of the settings page: who made this,
// where the source is, and under what licence.
//
// # Why it is not a setting
//
// It sits on the settings page and is not in the settings table, which
// is the distinction worth keeping. A setting is a decision somebody
// made; this is a fact about the build. Putting "who wrote this" in a
// table somebody can write to would produce a panel that can be made to
// say it was written by somebody else - and the first thing anyone
// checks after a compromise is what the software claims about itself.
//
// So it is compiled in, beside the version, and nothing can edit it at
// runtime.
//
// # Why every viewer sees it
//
// Not behind developer mode and not behind a capability. Two reasons:
// the licence and its attribution are things a customer is entitled to
// read without asking anybody, and a credits block that hides from the
// people running the software is not a credit.

// RepositoryURL is where the source is.
//
// A link, not a fetch. Nothing on this page loads anything from
// github.com - the Content-Security-Policy would refuse it, and that
// refusal is a feature this project has already paid for. An <a href>
// is navigation the reader chooses, which is a different thing.
const RepositoryURL = "https://github.com/cruciblelab/Crucible-Analytic"

// LicenceName is what LICENSE says, in the form people search for.
const LicenceName = "Apache-2.0"

// claudeMarkReason records why one of the three marks below is a plain
// letter rather than a logo.
//
// Anthropic's logo is their trademark. Apache-2.0 section 6 is explicit
// that a copyright licence grants no trademark rights, and
// THIRD-PARTY.md already makes exactly this point in the other
// direction, about CrucibleLAB's own branding. Beyond the licence,
// putting a vendor's logo on a product page is how a reader concludes
// the vendor endorsed the product, which nobody has said.
//
// Written down rather than left as a silent omission, because the
// obvious reading of a missing logo is that somebody forgot.
const claudeMarkReason = "Anthropic's mark is their trademark; Apache-2.0 section 6 " +
	"grants no trademark rights, and a vendor's logo on a product page reads as " +
	"that vendor endorsing the product. The name is the attribution."

// contributor is one row of the credits.
type contributor struct {
	// Name as it appears, and as CLA-SIGNATURES.md spells it. Not a
	// coincidence: aboutContributorsMatchTheSignatures asserts it.
	Name string
	// RoleKey is a catalogue key, so the role is translated and the
	// name is not.
	RoleKey string
	// Mark is the asset name of the badge, or "" for no badge.
	Mark string
}

// contributors is the credits list.
//
// Ordered by what a reader is looking for: the project, then the person
// answerable for it, then the tool. Not alphabetical - alphabetical
// ordering of three names is a decision that looks like an accident.
var contributors = []contributor{
	{Name: "CrucibleLAB", RoleKey: "ayarlar.hakkinda.rol.proje", Mark: "marka-cruciblelab.svg"},
	{Name: "Fırat Coşkun", RoleKey: "ayarlar.hakkinda.rol.gelistirici", Mark: "marka-firat.svg"},
	{Name: "Claude", RoleKey: "ayarlar.hakkinda.rol.asistan", Mark: "marka-claude.svg"},
}

// team are people in CrucibleLAB who have not signed the agreement.
//
// A separate list from contributors, and the separation is the point
// rather than a nicety. "Contributor" in this repository is a word with
// paperwork behind it: CLA-SIGNATURES.md records who agreed that their
// work may be in the software, and TestEveryContributorHasSignedTheCLA
// holds the panel's credits to that record.
//
// # Being on the team is not having signed
//
// The two are easy to conflate and the conflation is expensive in one
// specific direction. CLA.md exists to keep the licensing decision
// possible; the day code from an unsigned person merges, that decision
// needs their written permission, and a small team is exactly where
// nobody thinks to ask because everyone already knows each other.
//
// Naming somebody as a contributor who has not signed would mean either
// a red test or a signature written on their behalf, and the second is
// inventing a legal record - which is worse than a missing one, because
// a missing one is discovered while people are still reachable.
//
// The guard is already in place and does not depend on this file:
// internal/docs/cla_test.go compares git's commit authors against
// CLA-SIGNATURES.md, so the first commit either of these people makes
// turns that test red until they have signed. That is the test working,
// not a nuisance - and when they sign, they move up one list.
var team = []contributor{
	{Name: "Mehmet Can", RoleKey: "ayarlar.hakkinda.rol.ekip"},
	{Name: "Ali Hüseyin", RoleKey: "ayarlar.hakkinda.rol.ekip"},
}

// aboutRow is one contributor, ready to draw.
type aboutRow struct {
	Name string
	Role string
	// MarkURL is the hashed asset URL, or "" when the asset is missing.
	//
	// Empty rather than a broken image: assets are resolved through the
	// same map the stylesheet uses, and a name that is not in it means
	// the file was not embedded. A page that draws a broken image tells
	// the reader the panel is broken; one that draws the name alone
	// tells them nothing untrue.
	MarkURL string
}

// aboutSection is what the settings template draws.
type aboutSection struct {
	Product      string
	Version      string
	Repository   string
	Licence      string
	Contributors []aboutRow
	// Team is drawn under its own heading. Empty is a real state and
	// the template omits the heading rather than drawing an empty list.
	Team []aboutRow
}

// aboutFor builds the section.
func (s *Server) aboutFor(lang *ui.Language) aboutSection {
	out := aboutSection{
		Product:    lang.T("uygulama.ad"),
		Version:    buildinfo.Version(s.Renderer.Version),
		Repository: RepositoryURL,
		Licence:    LicenceName,
	}
	out.Contributors = s.rowsFor(lang, contributors)
	out.Team = s.rowsFor(lang, team)
	return out
}

// rowsFor turns one list into drawable rows.
func (s *Server) rowsFor(lang *ui.Language, list []contributor) []aboutRow {
	var out []aboutRow
	for _, c := range list {
		row := aboutRow{Name: c.Name, Role: lang.T(c.RoleKey)}
		if assets := s.Renderer.Assets(); c.Mark != "" && assets != nil {
			row.MarkURL = assets.URL(c.Mark)
		}
		out = append(out, row)
	}
	return out
}
