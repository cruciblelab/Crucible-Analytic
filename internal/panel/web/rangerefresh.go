package web

import (
	"errors"
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
)

// The "refresh the IP datasets now" button, on the health page.
//
// # Why here rather than in settings
//
// The same division L3 drew and for the same reason: the *policy* is
// configuration and the *action* belongs beside the fact it changes.
// Which dataset to fetch is a setting and lives in Ayarlar → Toplama;
// whether the last fetch worked is a fact about this deployment right
// now, which is what this page is.
//
// It also puts the button beside its own result. M3's done criterion is
// that the result shows on screen, and the result is the fetch log - so
// a button three pages away from it would have to be followed by "now go
// and look over there".

// rangeRefreshSection is what the health page draws.
type rangeRefreshSection struct {
	// Show is false when there is nothing to say: nothing has ever
	// fetched and nobody has ever asked. A permanent panel about a
	// feature that is switched off is a thing people learn to scroll
	// past, and this page is read precisely when somebody suspects
	// something is wrong.
	Show bool

	// Allowed is whether this actor may press the button.
	Allowed bool

	// Latest is the most recent request, and Unanswered is whether it
	// has been waiting long enough that nothing is going to take it.
	Latest     *rangerefresh.Request
	Unanswered bool

	// Files is the last attempt at each dataset file. The result.
	Files []panel.RangeFetch

	// AnyFailed is whether any file's most recent attempt failed, so the
	// page can lead with that rather than making the reader scan a
	// table for the one red row.
	AnyFailed bool

	// Notice is the sentence after a press.
	Notice string
	Failed bool
}

// rangeRefreshStatusFor builds the section.
func (s *Server) rangeRefreshStatusFor(r *http.Request, lang *ui.Language,
	access panel.Access) (rangeRefreshSection, string) {

	status, err := s.Store.RangeRefreshStatus(r.Context(), access)
	if err != nil {
		// The section is simply not drawn, and the sentence comes from
		// the catalog rather than from err. Every part of this page fails
		// independently - that is the page's whole reason for existing -
		// and a database message in English on a Turkish page is how a
		// panel stops being trusted. The detail is logged.
		s.logger().Warn("panel: could not read the range refresh status", "err", err)
		return rangeRefreshSection{}, lang.T("saglik.kaynak.okunamadi")
	}

	out := rangeRefreshSection{
		Allowed:    status.Allowed,
		Latest:     status.Latest,
		Unanswered: status.Unanswered(),
		Files:      status.Files,
	}
	for _, f := range status.Files {
		if f.Failed() {
			out.AnyFailed = true
			break
		}
	}
	// Drawn once anything has happened: a fetch has been recorded, or
	// somebody has asked for one. A deployment with asn_lookup off has
	// neither and sees nothing, which is right - the feature is not on.
	out.Show = len(status.Files) > 0 || status.Latest != nil
	return out, ""
}

// rangeRefreshPost handles the button.
//
// Returns the section to draw. The handler renders once on every path,
// for the reason settingsHandler records: a handler with two render
// sites grows a third.
func (s *Server) rangeRefreshPost(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access) (rangeRefreshSection, string) {

	// The operation record opens before the work, so its id can reach
	// the log lines the work produces. See internal/panel/operations.go.
	op, opErr := s.Store.BeginOperation(r.Context(), access,
		panel.ActionRangeRefreshRequested, "ip ranges", "")
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the refresh", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	// No developer password is read here, and that is deliberate rather
	// than missing - internal/panel's RequestRangeRefresh says why at
	// length. In one line: this makes work for nobody, so it is
	// entitlement rather than password.
	req, err := s.Store.RequestRangeRefresh(r.Context(), access, op.ID())

	section, sectionErr := s.rangeRefreshStatusFor(r, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}

	if err != nil {
		section.Notice = rangeRefreshErrorText(lang, err)
		section.Failed = true
		log.Warn("panel: range refresh request refused", "err", err)
		op.Step("istek yaz", false, "")
		notRolledBack := false
		_ = op.Finish(r.Context(), outcomeFor(err), err, &notRolledBack)
		return section, ""
	}

	// Drawn even when nothing has ever fetched, because the row now
	// exists and the reader has to be able to see it sitting there.
	section.Show = true
	section.Latest = req
	section.Notice = lang.T("saglik.kaynak.istendi")
	log.Info("panel: range refresh requested", "request", req.ID)
	op.Step("istek yaz", true, "")
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, nil)
	return section, ""
}

// rangeRefreshErrorText turns a refusal into a sentence for the person
// who pressed the button.
//
// Each case says what to do next, which is the only thing the reader
// wants. A refusal that describes itself and stops - "forbidden" - sends
// somebody to ask us what it meant.
func rangeRefreshErrorText(lang *ui.Language, err error) string {
	switch {
	case panel.IsRangeRefreshBusy(err):
		return lang.T("saglik.kaynak.zaten")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("saglik.kaynak.yetkisiz")
	default:
		// Deliberately not err.Error(), for the reason upgradeErrorText
		// records: everything above is a sentence somebody can act on,
		// and anything here is a database message in English. The detail
		// is in the operation record.
		return lang.T("saglik.kaynak.hata")
	}
}
