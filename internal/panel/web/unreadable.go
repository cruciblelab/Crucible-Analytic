package web

import (
	"errors"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The sentence a reader gets when a number could not be fetched, and the
// line the operator gets, produced by the same call.
//
// # Why they are one call - and what it cost to find out
//
// The page used to be the only audience. A failed analytics call became
// "Okunamadı." on the page and the error itself was dropped, in three
// separate places, none of which logged anything: the dashboard's
// page-wide notice, a breakdown's own notice, and the technical list.
// So a customer could report a panel that would not draw its countries
// and an operator had nothing whatever to look at - no line, no error
// code, no timing, not even which call it was.
//
// That is not a hypothetical, and the cost was the developer's own. The
// symptom sat in the working notes as "watch this" for two days: seen
// twice in a screenshot of the dashboard, both times undiagnosable,
// because nothing in the product records it. The diagnosis eventually
// came from timing the whole screenshot run - 37,85 s cold against
// 6,97 s warm - which is a measurement of the test rig, not of the
// deployment where it will happen next.
//
// A diagnosis nothing can reach is a diagnosis the product does not
// have.
//
// So: this is the only place either message key is written. A page that
// wants to tell a reader "could not be read" cannot do it without
// telling the log why, and TestEveryUnreadableMessageIsLogged holds that
// by reading the source rather than by trusting this comment.
//
// # Why Warn and not Error
//
// The service is fine and the deployment is fine; one call did not
// answer in time, and the page said so honestly. Error is for what
// stops working. This is for what somebody will ask about.
//
// # Why the range is in the line
//
// Because the first thing worth knowing is whether it fails on every
// range or only the long ones, and that is the difference between a
// service that is down and a query that is too slow. Both look
// identical on the page.
func (s *Server) unreadable(lang *ui.Language, where, siteID string,
	from, to time.Time, err error) string {

	key := "pano.hata.ulasilamiyor"
	if errors.Is(err, analytics.ErrRefused) {
		key = "pano.hata.reddedildi"
	}
	s.logger().Warn("panel: a section could not be read, and the page says so",
		"where", where, "site", siteID,
		"from", from.UTC().Format(time.RFC3339),
		"to", to.UTC().Format(time.RFC3339),
		"span", to.Sub(from).Round(time.Minute).String(),
		"err", err)
	return lang.T(key)
}
