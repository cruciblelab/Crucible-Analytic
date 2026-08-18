package web

import (
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The technical door: how an owner reaches the wizard their developer
// used.
//
// The awkward part of this feature is that both obvious designs are
// wrong.
//
// Hiding the technical wizard from the customer entirely is wrong
// because **the server is theirs.** A technically capable owner who
// wants to look at their own retention policy should not have to ask us
// for permission, and a product that makes them is a product they are
// right to distrust.
//
// Leaving it plainly linked is also wrong, because the common case is
// not a capable owner: it is somebody curious clicking through a menu,
// and reconfiguring a working installation is a support call at best.
//
// So it is neither hidden nor open. It is a door with a sentence on it
// saying the work behind it is already done, and one confirmation. The
// confirmation costs a technical owner four seconds and stops the
// accidental visit entirely.
//
// What it does **not** do is hand over the settings with legal weight.
// Those stay behind the developer password, which is a separate
// mechanism and unchanged: getting into the wizard is not the same as
// being able to change what is in it.

// TechnicalDoorPath is the confirmation page.
const TechnicalDoorPath = "/teknik"

// technicalDoorPage is Data for the confirmation template.
type technicalDoorPage struct {
	// SetupURL is where confirming leads.
	SetupURL string
	// AlreadyOpen reports that this principal may already walk in, so
	// the page offers a link rather than a warning. True for the
	// operator, who is not the reader this warning was written for.
	AlreadyOpen bool
}

// technicalDoorHandler shows the warning and records the decision.
func (s *Server) technicalDoorHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// Owners and the operator, and nobody else. An admin runs a site
	// day to day; the technical wizard is about the machine underneath
	// it, and a viewer has no business here at all.
	if !s.ownsAnySite(r.Context(), p) {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.Renderer.Render(w, r, http.StatusOK, "teknik", s.doorPage(r, lang, p))
	case http.MethodPost:
		if !s.acceptPost(w, r, lang) {
			return
		}
		// Recorded before the door opens, not after. Somebody who
		// reconfigures a working installation and then cannot explain
		// what happened is exactly who this line is for, and an entry
		// written afterwards is one a crash loses.
		_ = s.Store.RecordFor(r.Context(), p, panel.AuditEntry{
			Action: panel.ActionTechnicalDoorOpened,
		})
		s.Sessions.OpenTechnicalDoor(r.Context())
		http.Redirect(w, r, SetupPathPrefix+wizardSteps[0].ID, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) doorPage(r *http.Request, lang *ui.Language, p panel.Principal) *ui.Page {
	page := s.page(r, lang, panel.Access{Principal: p}, "teknik", lang.T("teknik.baslik"))
	page.Data = technicalDoorPage{
		SetupURL:    SetupPathPrefix + wizardSteps[0].ID,
		AlreadyOpen: s.Sessions.TechnicalDoorOpen(r.Context()),
	}
	return page
}
