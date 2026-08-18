package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The owner's side of developer access.
//
// C2 built the developer's side: a link minted from a shell, inert until
// somebody approves it, dead the moment the deployment acquires an
// owner. What was missing is the person who does the approving. Until
// this page existed the rule "once an account exists, the owner must
// consent" was true and unreachable - the request sat in a table nobody
// could see, and the only way through it was a SQL client.
//
// Three things this page has to get right, and only the first is
// obvious.
//
// **A developer may not approve developer access.** A redeemed link
// produces a principal with Superadmin set, because a developer needs to
// reach every site to do the work. ownsAnySite therefore answers "yes"
// for them - correctly, for the technical wizard, which is their tool.
// Here it would be a hole big enough to drive the whole mechanism
// through: an approved developer could approve the next request, and the
// next, and the owner would be asked exactly once, ever. So the guard on
// this page is not ownsAnySite. It is "a signed-in **person** who owns
// something", and the developer is refused before ownership is even
// considered.
//
// **The panel does not know who asked.** There is no name to show. A
// request is minted by somebody with a shell on the server, and the
// reason attached to it is a sentence that person typed. The page says
// so, in those words, rather than presenting an unverified string as an
// identity. An owner deciding whether to let somebody into their
// customers' data deserves to know exactly how much the panel actually
// checked, which is: nothing.
//
// **An auto-approved link is dead here, whatever the row says.** A
// bootstrap grant carries approved_at, because during installation there
// was nobody to ask. RedeemDevAccess refuses it the instant an account
// exists. This page is only reachable by somebody signed in, so an
// account always exists - which means every auto_approved row on it is
// already spent, and drawing it as "approved" would tell the owner
// somebody can still walk in.

// DevAccessRequestsPath is the owner's approval page.
//
// Deliberately not under the redemption prefix. /gelistirici/{token} is
// a public URL that hands out a session; this is a page behind the login
// form. Two routes that do very different things should not look like
// neighbours.
const DevAccessRequestsPath = "/erisim"

// recentDevAccessLimit bounds the history table.
//
// Requests are purged after thirty days, so this is not paging - it is a
// ceiling on a page that is meant to be read, not audited. The audit log
// is where "everything, forever" lives.
const recentDevAccessLimit = 20

// devAccessState is the closed set of states a request is drawn in.
//
// Closed because the value is concatenated into a catalog key. A state
// derived from a row rather than typed by anybody is still a string
// reaching a lookup, and this project's rule is that such strings are
// types.
type devAccessState string

const (
	stateWaiting   devAccessState = "bekliyor"
	stateApproved  devAccessState = "onaylandi"
	stateDenied    devAccessState = "reddedildi"
	stateUsed      devAccessState = "kullanildi"
	stateExpired   devAccessState = "doldu"
	stateBootstrap devAccessState = "kurulum"
)

// devAccessRow is one request as the owner sees it.
type devAccessRow struct {
	ID     int64
	Reason string
	// State decides the badge and the sentence beside it.
	State      devAccessState
	StateLabel string
	// Decidable reports whether the approve and deny buttons are drawn.
	// Computed from the same question the POST handler asks again.
	Decidable bool

	RequestedAt      time.Time
	RequestExpiresAt time.Time
	SessionTTL       time.Duration

	// DecidedBy names the person who approved it, when one did. Empty
	// for a bootstrap grant, which nobody consented to.
	DecidedBy string
	// UsedAt is the zero time when the link was never redeemed, rather
	// than a nil pointer as the store returns. A template cannot pass a
	// *time.Time to a formatter that takes a time.Time, and discovering
	// that at render time - on a page that only draws its interesting
	// half once somebody has actually used a link - is exactly the sort
	// of defect that reaches production.
	UsedAt time.Time
	// UsedFrom is where the link was redeemed from, as text. The address
	// the connection came from and never a forwarded header - see
	// peerAddr.
	UsedFrom string
}

// devAccessPage is Data for the template.
type devAccessPage struct {
	Pending []devAccessRow
	Recent  []devAccessRow
	// Empty means nothing has ever been requested on this deployment,
	// which wants a different sentence from "nothing is pending".
	Empty bool

	Message string
	Failed  bool
}

// devAccessRequestsHandler serves and processes the approval page.
func (s *Server) devAccessRequestsHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if !s.haveStore(w, r, lang) {
		return
	}
	p, decider, ok := s.requireDecider(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderDevAccess(w, r, lang, p, devAccessPage{})
	case http.MethodPost:
		if !s.acceptPost(w, r, lang) {
			return
		}
		s.saveDecision(w, r, lang, p, decider)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

// requireDecider resolves somebody entitled to decide developer access.
//
// **Kind before ownership.** A developer principal carries Superadmin,
// because a developer has to reach every site to do the work, so
// ownsAnySite answers "yes" for them. Asking that question first would
// let an approved developer approve the next request, and the next, and
// the owner would be asked exactly once - ever.
//
// Removing the Kind check does not actually open that hole today, and
// the honest version of this comment has to say so: the next line loads
// the deciding User by id, a developer principal has no user id, and the
// load fails. The escalation is blocked either way. What the Kind check
// buys is that it is blocked *on purpose*, with the right status - 403
// rather than a 500 from a lookup nobody meant to depend on. A rule
// enforced by an accident downstream is a rule that disappears the day
// somebody makes that accident stop happening.
//
// 403 rather than 404 for everybody refused here. This page is not a
// secret - an admin knows the deployment has owners and knows what
// developer access is - and "you are not the one who decides this" is a
// more useful answer than pretending the page is not there.
//
// The User row comes back because approving records who consented, by
// value, and a Principal carries a label rather than the row.
func (s *Server) requireDecider(w http.ResponseWriter, r *http.Request) (panel.Principal, panel.User, bool) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return panel.Principal{}, panel.User{}, false
	}
	if p.Kind != panel.PrincipalUser {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return panel.Principal{}, panel.User{}, false
	}
	if !s.ownsAnySite(r.Context(), p) {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return panel.Principal{}, panel.User{}, false
	}

	user, err := s.Store.UserByID(r.Context(), p.UserID)
	if err != nil {
		s.logger().Error("panel: loading decider", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return panel.Principal{}, panel.User{}, false
	}
	return p, user, true
}

func (s *Server) saveDecision(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, by panel.User) {

	ctx := r.Context()
	id, ok := parsePositiveID(r.PostFormValue("istek"))
	if !ok {
		s.renderDevAccess(w, r, lang, p, devAccessPage{
			Message: lang.T("erisim.hata.istek_gecersiz"), Failed: true})
		return
	}

	var (
		err  error
		done string
	)
	switch r.PostFormValue("islem") {
	case "onayla":
		err = s.Store.ApproveDevAccess(ctx, id, by)
		done = "erisim.onaylandi"
	case "reddet":
		err = s.Store.DenyDevAccess(ctx, id, by)
		done = "erisim.reddedildi"
	default:
		s.renderDevAccess(w, r, lang, p, devAccessPage{
			Message: lang.T("erisim.hata.bilinmeyen"), Failed: true})
		return
	}

	if err != nil {
		s.renderDevAccess(w, r, lang, p, s.decisionFailed(lang, err))
		return
	}
	s.renderDevAccess(w, r, lang, p, devAccessPage{Message: lang.T(done)})
}

// decisionFailed turns a store error into a sentence.
//
// The already-decided refusal is the reason this exists rather than one
// generic message. Two owners looking at the same banner and both
// clicking is the ordinary way to reach it - the UPDATE's own WHERE
// clause settles which one wins - and telling the loser "that request
// has already been decided" is the truth, where "could not be saved"
// would send them to look for a fault.
func (s *Server) decisionFailed(lang *ui.Language, err error) devAccessPage {
	if errors.Is(err, panel.ErrDevAccessDecided) {
		return devAccessPage{Message: lang.T("erisim.hata.karar_verilmis"), Failed: true}
	}
	s.logger().Error("panel: deciding developer access", "err", err)
	return devAccessPage{Message: lang.T("erisim.hata.kaydedilemedi"), Failed: true}
}

func (s *Server) renderDevAccess(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, data devAccessPage) {

	ctx := r.Context()
	recent, err := s.Store.RecentDevAccess(ctx, recentDevAccessLimit)
	if err != nil {
		s.logger().Error("panel: listing developer access", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	now := time.Now()
	for _, req := range recent {
		row := devAccessRowFor(lang, req, now)
		if row.Decidable {
			data.Pending = append(data.Pending, row)
			continue
		}
		data.Recent = append(data.Recent, row)
	}
	data.Empty = len(recent) == 0

	page := s.page(r, lang, panel.Access{Principal: p}, "erisim", lang.T("erisim.baslik"))
	page.Data = data
	if data.Message != "" {
		level := ui.NoticeInfo
		if data.Failed {
			level = ui.NoticeError
		}
		page.Notices = append(page.Notices, ui.Notice{Level: level, Body: data.Message})
	}
	status := http.StatusOK
	if data.Failed {
		status = http.StatusBadRequest
	}
	s.Renderer.Render(w, r, status, "erisim", page)
}

// devAccessRowFor decides how one request is drawn.
//
// The order of the tests is the order of what actually happened, most
// final first: a link that was used stays "used" even after its window
// closes, and a denial is not undone by time passing. Reversing any pair
// here produces a row that tells the owner a different story from the
// one the database holds.
func devAccessRowFor(lang *ui.Language, req panel.DevAccessRequest, now time.Time) devAccessRow {
	row := devAccessRow{
		ID:               req.ID,
		Reason:           req.Reason,
		RequestedAt:      req.RequestedAt,
		RequestExpiresAt: req.RequestExpiresAt,
		SessionTTL:       req.SessionTTL,
		DecidedBy:        req.ApprovedLabel,
	}
	if req.UsedAt != nil {
		row.UsedAt = *req.UsedAt
	}
	if req.UsedFrom != nil {
		row.UsedFrom = req.UsedFrom.String()
	}

	switch {
	case req.AutoApproved:
		// Whatever else is true of it. This page cannot be reached
		// without an account existing, and an account existing is
		// precisely what kills a bootstrap grant - so drawing one as
		// "approved and waiting" would say somebody can still walk in.
		row.State = stateBootstrap
		row.DecidedBy = ""
	case req.UsedAt != nil:
		row.State = stateUsed
	case req.DeniedAt != nil:
		row.State = stateDenied
	case req.ApprovedAt != nil:
		row.State = stateApproved
	case now.After(req.RequestExpiresAt):
		row.State = stateExpired
	default:
		row.State = stateWaiting
		// The only state with buttons. Drawn from the same condition the
		// store's UPDATE enforces, so the page cannot offer a decision
		// the database will refuse.
		row.Decidable = true
	}
	row.StateLabel = lang.T("erisim.durum." + string(row.State))
	return row
}
