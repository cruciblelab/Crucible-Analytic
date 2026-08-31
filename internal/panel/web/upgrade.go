package web

import (
	"errors"
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// The upgrade button, on the health page.
//
// # Why here and not in settings
//
// The page already shows the two numbers the button acts on - the schema
// the database carries and the one this build expects - and L1 put them
// there rather than in settings for a reason that still holds: they are
// facts about the deployment, not preferences. A button that acted on
// them from three pages away would make the reader carry the comparison
// in their head.
//
// The lock that governs the button is a setting, and lives there. The
// division is the honest one: the *policy* is configuration, the
// *action* belongs beside the fact it changes.

// upgradeSection is what the health page draws.
type upgradeSection struct {
	// Show is false when there is nothing to say - the schema matches
	// and nothing has ever been requested. A permanent "everything is
	// fine" panel is a thing people learn to scroll past, and this page
	// is read precisely when somebody suspects it is not.
	Show bool

	// Needed, Locked and Allowed come straight from the store; see
	// panel.UpgradeStatus for what each means.
	Needed  bool
	Locked  bool
	Allowed bool

	// AskingForPassword is true when the lock is on and the actor is
	// otherwise entitled: the form then draws a password field rather
	// than refusing outright, because the developer is a person who may
	// well be standing there.
	AskingForPassword bool

	// Latest is the most recent request, and the fields the page reads
	// off it: what state it is in and what went wrong.
	Latest *upgrade.Request

	// Notice is the sentence after a press.
	Notice string
	Failed bool
}

// upgradeStatusFor builds the section.
func (s *Server) upgradeStatusFor(r *http.Request, lang *ui.Language, access panel.Access) (upgradeSection, string) {
	status, err := s.Store.UpgradeStatus(r.Context(), access)
	if err != nil {
		// The section is simply not drawn. Every part of this page fails
		// independently - that is the page's entire reason for existing
		// - so an unreadable upgrade state must not take the storage
		// figures down with it.
		//
		// A sentence from the catalog rather than err.Error(), for the
		// reason upgradeErrorText records two functions below: a
		// database message in English, printed on a Turkish page in
		// place of an explanation, is how a panel stops being trusted.
		// The detail is logged.
		s.logger().Warn("panel: could not read the upgrade status", "err", err)
		return upgradeSection{}, lang.T("saglik.yukselt.okunamadi")
	}

	out := upgradeSection{
		Needed:  status.Needed,
		Locked:  status.Locked,
		Allowed: status.Allowed,
		Latest:  status.Latest,
	}
	out.AskingForPassword = status.Needed && status.Locked && access.Can(panel.CapManageSettings)
	out.Show = status.Needed || (status.Latest != nil && status.Latest.InFlight())
	return out, ""
}

// upgradePost handles the button.
//
// Returns the section to draw. The handler renders once on every path,
// for the reason settingsHandler records: a handler with two render
// sites grows a third.
func (s *Server) upgradePost(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access) (upgradeSection, string) {

	// The operation record opens before the work, so its id can reach
	// the log lines the work produces. See internal/panel/operations.go.
	op, opErr := s.Store.BeginOperation(r.Context(), access,
		panel.ActionUpgradeRequested, "schema", "")
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the upgrade", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	var auth devgate.Authorization
	if s.Gate != nil {
		// Verified whatever the lock says. Checking the lock first and
		// only then reading the password would leak the lock's state to
		// anybody who could watch the timing, and it is one hash either
		// way.
		result := s.Gate.Verify(r.Context(), devgate.RequestFrom(r,
			access.Principal.Label, panel.UpgradeGateAction))
		if result.OK() {
			auth = result.For(panel.UpgradeGateAction)
		}
	}

	req, err := s.Store.RequestUpgrade(r.Context(), access, auth, op.ID())

	section, sectionErr := s.upgradeStatusFor(r, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}

	if err != nil {
		section.Notice = upgradeErrorText(lang, err)
		section.Failed = true
		log.Warn("panel: upgrade request refused", "err", err)
		op.Step("istek yaz", false, "")
		notRolledBack := false
		_ = op.Finish(r.Context(), outcomeFor(err), err, &notRolledBack)
		return section, ""
	}

	section.Notice = lang.T("saglik.yukselt.istendi")
	section.Latest = req
	log.Info("panel: upgrade requested", "request", req.ID)
	op.Step("istek yaz", true, "")
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, nil)
	return section, ""
}

// upgradeErrorText turns a refusal into a sentence for the person who
// pressed the button.
//
// Each case says what to do next, which is the only thing the reader
// wants. A refusal that describes itself and stops - "forbidden" - sends
// somebody to ask us what it meant.
func upgradeErrorText(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, panel.ErrUpgradeLocked):
		return lang.T("saglik.yukselt.kilitli")
	case errors.Is(err, panel.ErrUpgradeNotNeeded):
		return lang.T("saglik.yukselt.gerekmiyor")
	case errors.Is(err, upgrade.ErrAlreadyInFlight):
		return lang.T("saglik.yukselt.zaten")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("saglik.yukselt.yetkisiz")
	default:
		// Deliberately not err.Error(). Everything above is a sentence
		// somebody can act on; anything here is a database message in
		// English, and putting it on a Turkish page in place of an
		// explanation is how a panel stops being trusted. The detail is
		// in the operation record.
		return lang.T("saglik.yukselt.hata")
	}
}
