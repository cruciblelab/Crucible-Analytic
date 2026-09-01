package panel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
)

// The "refresh the IP datasets now" button's rules.
//
// Here rather than in the handler, for the reason D4a learned the
// expensive way: a rule that lives in an HTTP handler is a rule the next
// entry point does not have. The handler's only job is to turn the
// answer into a page.
//
// # Why there is no lock, and no developer password
//
// L3's upgrade button has both, and the difference is worth stating
// rather than leaving as an inconsistency somebody later "fixes".
//
// The project's rule is in SOZLUK.md §3: anything that can make work for
// the developer sits behind the developer password, because a customer
// can grant themselves any capability - RoleOwner holds CapManageMembers
// and can make somebody an admin - so a capability is not a limit on the
// customer, only on their staff.
//
// Pressing this makes work for nobody. It re-downloads two public
// datasets onto the customer's own server, over the customer's own
// link, and rewrites two of the customer's own tables. Nothing about it
// reaches us, and the worst outcome of pressing it repeatedly is that
// the customer's server does the work it does every week anyway.
//
// So it is entitlement only: whoever may change settings may press it.
// A password here would be ceremony that teaches people the password is
// asked for things that do not matter.
//
// A cooldown was considered and left out. The in-flight index already
// means one refresh at a time, which is the criterion M3 states; a
// minimum gap on top of that would be an unmeasured number bounding a
// cost nobody has measured either.

// RangeRefreshStatus is what the page shows.
type RangeRefreshStatus struct {
	// Allowed is whether this actor could press it right now.
	Allowed bool

	// Latest is the most recent request, nil when there has never been
	// one.
	Latest *rangerefresh.Request

	// NothingIsFetching is the state a deployment with asn_lookup off is
	// in: no fetch has ever been recorded, so nothing is polling the
	// queue and a press would sit unanswered until it expired.
	//
	// Observed rather than asserted. The panel cannot read another
	// service's configuration file - it must not - so "is asn_lookup
	// enabled" is not a question it can ask. What it can see is whether
	// anything has ever fetched, which answers the question that
	// actually matters.
	NothingIsFetching bool

	// Files is the last attempt at each dataset file, newest per file.
	// The result M3 has to put on the screen; ip_range_fetches (M2) is
	// where it comes from.
	Files []RangeFetch
}

// RangeRefreshStatus reads everything the page needs in one call.
func (s *Store) RangeRefreshStatus(ctx context.Context, a Access) (RangeRefreshStatus, error) {
	var out RangeRefreshStatus
	out.Allowed = a.Can(CapManageSettings)

	files, err := s.LastRangeFetchPerFile(ctx)
	if err != nil {
		return out, fmt.Errorf("panel: range refresh status: %w", err)
	}
	out.Files = files
	out.NothingIsFetching = len(files) == 0

	latest, err := rangerefresh.Latest(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: range refresh status: %w", err)
	}
	out.Latest = latest
	return out, nil
}

// Unanswered reports whether the last request has been waiting long
// enough that nothing is going to take it.
//
// A method on the status rather than a stored field, because it is a
// question about *now*: a request that was fresh when the page was built
// is stale thirty seconds later, and a boolean frozen at query time
// would say the wrong thing on a page somebody left open.
func (st RangeRefreshStatus) Unanswered() bool {
	return st.Latest.Unclaimed(time.Now(), rangerefresh.UnclaimedAfter)
}

// RequestRangeRefresh asks for the datasets to be fetched now.
//
// # What it does before asking
//
// Expires unclaimed requests first, and that is not tidying. asn_lookup
// is off by default, so the ordinary state of most deployments is that
// nothing polls this queue at all - and the in-flight index would turn
// the first press into a button that is dead for ever. Clearing what
// nobody took is what keeps the second press possible.
//
// Only rows older than rangerefresh.UnclaimedAfter, which is four poll
// intervals: a request that has survived that was not going to be
// claimed. A shorter window would delete a request a fetcher was about
// to take, and the customer would see nothing happen for a reason no
// page could explain.
func (s *Store) RequestRangeRefresh(ctx context.Context, a Access,
	operationID string) (*rangerefresh.Request, error) {

	// Entitlement first, and before anything that costs work. A viewer
	// is refused on who they are.
	if !a.Can(CapManageSettings) {
		return nil, fmt.Errorf("%w (range refresh)", ErrSettingNotWritable)
	}

	// Not fatal when it fails: the ask below either succeeds or reports
	// ErrAlreadyInFlight, and both are better answers to give the person
	// standing there than refusing to try. The failure that matters -
	// a jammed queue - shows up as ErrAlreadyInFlight with a request the
	// page then displays as unanswered, which is the honest picture.
	_, _ = rangerefresh.ExpireStale(ctx, s.pool, rangerefresh.UnclaimedAfter)

	req, err := rangerefresh.Ask(ctx, s.pool, refreshActorFor(a.Principal), operationID)
	if err != nil {
		return nil, err
	}

	// Recorded after the row exists, so the audit log never claims a
	// request the in-flight index refused. The reverse ordering would be
	// wrong in the direction nobody checks.
	if _, auditErr := s.recordForReturningID(ctx, a.Principal, AuditEntry{
		Action: ActionRangeRefreshRequested,
		Target: "ip ranges",
		Detail: map[string]any{"request_id": req.ID},
	}); auditErr != nil {
		// The request is queued and will be answered. Failing the call
		// now would tell the customer it did not happen, which is the one
		// answer that is definitely wrong.
		return req, nil
	}
	return req, nil
}

// PurgeOldRangeRefreshRequests trims the queue.
//
// Thirty days, matching panel_operations: a request record is read
// "usually within the hour" and a month covers the slow version of that
// conversation. The audit log keeps the fact that somebody asked.
//
// Finished rows only. A pending or running one is either waiting for a
// fetcher or being worked on, and deleting it would free the in-flight
// slot underneath whoever holds it.
func (s *Store) PurgeOldRangeRefreshRequests(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM ip_range_refresh_requests
		WHERE state IN ('succeeded', 'failed')
		  AND requested_at < now() - $1::interval`, seconds(rangeRefreshRetention))
	if err != nil {
		return 0, fmt.Errorf("panel: purge range refresh requests: %w", err)
	}
	return tag.RowsAffected(), nil
}

// refreshActorFor converts a principal into the shape this queue records.
//
// A second converter beside actorFor, because the two queues have their
// own Actor types on purpose. Sharing one would couple internal/upgrade
// and internal/rangerefresh to each other for the sake of three fields,
// and the first time one of them needed a fourth the coupling would be
// the thing in the way.
func refreshActorFor(p Principal) rangerefresh.Actor {
	out := rangerefresh.Actor{Kind: string(p.Kind), Label: p.Label}
	if p.UserID != 0 {
		id := p.UserID
		out.ID = &id
	}
	return out
}

// ErrRangeRefreshBusy is ErrAlreadyInFlight, re-exported so the web
// package does not have to import the queue to name the one error it
// shows a sentence for.
var ErrRangeRefreshBusy = rangerefresh.ErrAlreadyInFlight

// IsRangeRefreshBusy reports whether err is the "one is already going"
// refusal.
func IsRangeRefreshBusy(err error) bool {
	return errors.Is(err, rangerefresh.ErrAlreadyInFlight)
}
