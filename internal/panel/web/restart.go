package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The restart button, on the health page (B3e, catalogue #38).
//
// Beside the services table rather than in settings, for the reason the
// upgrade section gives: the page already shows the fact the button acts
// on - which service is reporting, since when - and a button three pages
// away would make the reader carry that in their head.
//
// What the section draws is not "restarted" because somebody pressed. It
// draws what the service did: the request's time against the start the
// service itself reported, compared by restartreq.Satisfied - the same
// sentence the service exits on. See panel.RestartStatus.

// restartFormField is the select the service arrives in.
const restartFormField = "servis"

// restartSection is the section as the page draws it.
type restartSection struct {
	// Allowed is panel.RestartStatus.Allowed: may this principal press
	// at all. The password is still asked every time.
	Allowed bool
	// AskingForPassword is Allowed on a deployment that has a developer
	// password. Without one the button could only ever be refused, and
	// the section says that instead of drawing it.
	AskingForPassword bool

	Rows []restartRow
	// Choices are the services the select offers: the askable ones,
	// derived from the same state the rows show - one decision, two
	// places it is read.
	Choices []string
	// Chosen is what was selected, echoed back after a refused press.
	// Empty selects the placeholder: nothing is chosen for anybody.
	Chosen string
	// Pending is true while any row is waiting, so the section refreshes
	// itself and the reader sees the restart land without reloading.
	Pending bool

	Notice string
	Failed bool
}

// restartRow is one service.
type restartRow struct {
	Name string
	// State is the word the template branches on; see panel.RestartState.
	State     panel.RestartState
	Requested time.Time
	Started   time.Time
}

// restartStatusFor builds the section.
//
// Takes the store rather than reading it off the server, and a context
// rather than a request, so the section can be built with neither a
// database nor an HTTP request. See stores.go.
func (s *Server) restartStatusFor(ctx context.Context, db restartReader, lang *ui.Language,
	access panel.Access) (restartSection, string) {

	status, err := db.RestartStatus(ctx, access)
	if err != nil {
		// The section alone goes dark. Every part of this page fails on
		// its own; the detail is logged, and the reader gets a sentence
		// from the catalog rather than a database message in English.
		s.logger().Warn("panel: could not read the restart status", "err", err)
		return restartSection{}, lang.T("saglik.yb.okunamadi")
	}

	out := restartSection{
		Allowed:           status.Allowed,
		AskingForPassword: status.Allowed && s.Gate != nil && s.Gate.Configured(),
	}
	for _, svc := range status.Services {
		out.Rows = append(out.Rows, restartRow{
			Name:      svc.Service,
			State:     svc.State,
			Requested: svc.Requested,
			Started:   svc.Started,
		})
		if svc.State.Askable() {
			out.Choices = append(out.Choices, svc.Service)
		}
		if svc.State == panel.RestartPending {
			out.Pending = true
		}
	}
	return out, ""
}

// restartPost handles the button.
func (s *Server) restartPost(r *http.Request, db restartStore, lang *ui.Language,
	access panel.Access) (restartSection, string) {

	service := r.FormValue(restartFormField)

	op, opErr := db.BeginOperation(r.Context(), access,
		panel.ActionRestartRequested, service, "")
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the restart", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	var auth devgate.Authorization
	if s.Gate != nil {
		result := s.Gate.Verify(r.Context(), devgate.RequestFrom(r,
			access.Principal.Label, panel.RestartGateAction))
		if result.OK() {
			auth = result.For(panel.RestartGateAction)
		}
	}

	req, err := db.RequestRestart(r.Context(), access, auth, service)

	// Read after the write, so the row the person just caused is the row
	// they see.
	section, sectionErr := s.restartStatusFor(r.Context(), db, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}
	if err != nil {
		// Echoed back only on a refusal, so the person does not have to
		// choose again. After an accepted press the select starts empty
		// again: the same service is refused for a minute anyway, and a
		// list left on it is one keystroke from asking twice.
		section.Chosen = service
		section.Notice = restartErrorText(lang, err)
		section.Failed = true
		log.Warn("panel: restart request refused", "err", err, "service", service)
		op.Step("istek yaz", false, "")
		_ = op.Finish(r.Context(), restartOutcome(err), err, nil)
		return section, ""
	}

	op.LinkAudit(r.Context(), req.AuditID)
	section.Notice = lang.Tf("saglik.yb.istendi", lang.T("saglik.servis."+req.Service))
	log.Info("panel: restart requested", "service", req.Service, "requested", req.At)
	op.Step("istek yaz", true, "")
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, nil)
	return section, ""
}

// restartOutcome is a refusal or a failure.
//
// Every refusal this store can give is the system working - a missing
// password, a service that cannot hear, a press inside the minute - and
// burying them among failures is how a real fault gets missed.
func restartOutcome(err error) panel.Outcome {
	switch {
	case errors.Is(err, panel.ErrSettingNotWritable),
		errors.Is(err, panel.ErrRestartNeedsPassword),
		errors.Is(err, panel.ErrRestartUnknownService),
		errors.Is(err, panel.ErrRestartCannotHear),
		errors.Is(err, panel.ErrRestartNeverReported),
		errors.Is(err, panel.ErrRestartTooSoon):
		return panel.OutcomeRefused
	default:
		return panel.OutcomeFailed
	}
}

// restartErrorText turns a refusal into a sentence that says what to do
// next.
func restartErrorText(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, panel.ErrRestartNeedsPassword):
		return lang.T("saglik.yb.parola_gerek")
	case errors.Is(err, panel.ErrRestartTooSoon):
		return lang.T("saglik.yb.cok_yakin")
	case errors.Is(err, panel.ErrRestartCannotHear):
		return lang.T("saglik.yb.duymaz_ret")
	case errors.Is(err, panel.ErrRestartNeverReported):
		return lang.T("saglik.yb.raporsuz_ret")
	case errors.Is(err, panel.ErrRestartUnknownService):
		return lang.T("saglik.yb.bilinmeyen")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("saglik.yb.yetkisiz")
	default:
		// Not err.Error(), for the reason upgradeErrorText records. The
		// detail is in the operation record.
		return lang.T("saglik.yb.hata")
	}
}
