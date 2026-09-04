package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

// V5: the surface for the update the last four phases built.
//
// V1 signed the packages, V2 queued the requests, V3 downloaded and
// verified them, V4 installed them and put the old ones back when they
// did not run. None of it was reachable from a browser: the queue had a
// table and no page, which is the state a customer experiences as "the
// feature does not exist".
//
// # Why this section is drawn even when nothing is pending
//
// The schema section hides itself when the schema matches, because a
// permanent "everything is fine" panel is something people learn to
// scroll past. This one cannot use that rule: there is no local fact
// that says whether a newer version exists. The panel does not know -
// see panel.RequestRelease for why it structurally cannot - so
// "nothing to do" is not a state it can be in.
//
// What it draws instead is the version that is running and the field to
// name another. That is the honest surface for a component that cannot
// see the shelf it is ordering from.

// releaseSection is the update panel on the health page.
type releaseSection struct {
	// Current is the version this binary was built as.
	Current string

	// Locked and Allowed come from panel.ReleaseStatus.
	Locked  bool
	Allowed bool

	// AskingForPassword is true when the lock is on and the actor is
	// otherwise entitled: the form draws a password field rather than
	// refusing outright, because the developer may well be standing
	// there. Same rule as the schema section, and it matters more here -
	// this lock is on by default, so the password path is the ordinary
	// one rather than the exception.
	AskingForPassword bool

	// Typed is what was in the version field, echoed back so a refused
	// request does not make somebody type it again.
	Typed string

	// Latest is the most recent request.
	Latest *relupdate.Request

	// Notice is the sentence after a press.
	Notice string
	Failed bool
}

// releaseStatusFor builds the section.
func (s *Server) releaseStatusFor(r *http.Request, lang *ui.Language,
	access panel.Access) (releaseSection, string) {

	current := buildinfo.Version(s.Renderer.Version)
	status, err := s.Store.ReleaseStatus(r.Context(), access, current)
	if err != nil {
		// Drawn as absent, not as broken. Every part of this page fails
		// independently - that is the page's whole reason for existing -
		// so an unreadable release queue must not take the storage
		// figures down with it.
		s.logger().Warn("panel: could not read the release status", "err", err)
		return releaseSection{}, lang.T("saglik.surum.okunamadi")
	}

	out := releaseSection{
		Current: status.Current,
		Locked:  status.Locked,
		Allowed: status.Allowed,
		Latest:  status.Latest,
	}
	out.AskingForPassword = status.Locked && access.Can(panel.CapManageSettings)
	return out, ""
}

// releasePost handles the button.
func (s *Server) releasePost(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access) (releaseSection, string) {

	typed := strings.TrimSpace(r.FormValue("surum"))

	// The operation record opens before the work, so its id reaches the
	// log lines the work produces - including the upgrader's, which runs
	// in another process and finds the id on the request row.
	op, opErr := s.Store.BeginOperation(r.Context(), access,
		panel.ActionReleaseRequested, "release", typed)
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the release", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	var auth devgate.Authorization
	if s.Gate != nil {
		// Verified whatever the lock says, for the reason upgradePost
		// gives: reading the password only when the lock is on leaks the
		// lock's state to anybody who can watch the timing, and it is one
		// hash either way.
		result := s.Gate.Verify(r.Context(), devgate.RequestFrom(r,
			access.Principal.Label, panel.ReleaseGateAction))
		if result.OK() {
			auth = result.For(panel.ReleaseGateAction)
		}
	}

	current := buildinfo.Version(s.Renderer.Version)
	req, err := s.Store.RequestRelease(r.Context(), access, auth, op.ID(), current, typed)

	section, sectionErr := s.releaseStatusFor(r, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}
	section.Typed = typed

	if err != nil {
		section.Notice = releaseErrorText(lang, err)
		section.Failed = true
		// The version is in the log line and not in the message: a
		// refusal the person can act on says what to do, and the string
		// they typed is already in the field in front of them.
		log.Warn("panel: release request refused", "err", err, "version", typed)
		op.Step("istek yaz", false, "")
		notRolledBack := false
		_ = op.Finish(r.Context(), outcomeFor(err), err, &notRolledBack)
		return section, ""
	}

	section.Notice = lang.Tf("saglik.surum.istendi", typed)
	section.Latest = req
	section.Typed = ""
	log.Info("panel: release requested", "request", req.ID, "version", typed)
	op.Step("istek yaz", true, "")
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, nil)
	return section, ""
}

// releaseErrorText turns a refusal into a sentence for the person who
// pressed the button.
//
// Every case says what to do next. A refusal that describes itself and
// stops - "forbidden" - sends somebody to ask us what it meant, which
// is the outcome this whole phase exists to avoid.
func releaseErrorText(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, panel.ErrReleaseLocked):
		return lang.T("saglik.surum.kilitli")
	case errors.Is(err, panel.ErrReleaseBadVersion), errors.Is(err, relupdate.ErrBadVersion):
		return lang.T("saglik.surum.gecersiz")
	case errors.Is(err, relupdate.ErrAlreadyInFlight):
		return lang.T("saglik.surum.zaten")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("saglik.surum.yetkisiz")
	default:
		// Deliberately not err.Error(), for the reason upgradeErrorText
		// records: a database message in English, printed on a Turkish
		// page in place of an explanation, is how a panel stops being
		// trusted. The detail is in the operation record.
		return lang.T("saglik.surum.hata")
	}
}
