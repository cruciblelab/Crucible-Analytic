package panel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/restartreq"
)

// Asking a service to restart, from the health page (B3e, catalogue #38).
//
// The mechanism is internal/restartreq's: the panel writes the database's
// clock into one settings row, and the service, seeing a request newer
// than its own start, exits cleanly for its supervisor to start it again.
// This file is the panel's half - who may write that row, how often, and
// what the page says afterwards.
//
// # Why the developer password, always
//
// The release and schema buttons can be unlocked; this one cannot. A
// restart of the collector takes the customer's website off the network
// for the seconds it takes to come back, and a restart of anything is
// the first thing somebody tries when the support call starts - so it is
// an operation that makes work for the developer, and by this product's
// access principle such an operation stands behind the developer
// password rather than behind a role. A role a customer can grant
// themselves would be no lock at all.
//
// The owner may *attempt* it, as with the other three password forms on
// the health page: the page is theirs, and the password is the
// authority, not the account that typed it.
//
// # Why one row per service, and why it is not a setting
//
// It is the table the panel already writes and every service able to
// hear a request already reads - restartreq.KeyPrefix says why in full.
// The key is in no registry, so the settings page never lists it.

// RestartGateAction is what an authorization must be minted for.
//
// Its own action, so a password typed to install a release does not also
// take the site down for a restart, and the other way round.
const RestartGateAction = "process:restart"

// RestartSpacing is how often one service may be asked.
//
// Once a minute. The supervisors this project ships restart a service
// that exits on its own, and systemd stops doing that after five starts
// inside its own sixty-second window (StartLimitBurst); one request a
// minute keeps a person pressing the button far from that edge, so the
// button can never leave a unit in the failed state its supervisor gives
// up on.
//
// It is also how long the page calls a request "waiting": the watcher
// asks every five seconds and a clean exit is bounded at ten, so a
// request a minute old that has not been honoured is not going to be by
// waiting longer - and that is exactly when the button becomes pressable
// again.
const RestartSpacing = time.Minute

var (
	// ErrRestartUnknownService is a service outside the closed list.
	ErrRestartUnknownService = errors.New("panel: that is not a service the panel can ask to restart")
	// ErrRestartNeedsPassword is a request without the developer password.
	ErrRestartNeedsPassword = errors.New("panel: asking a service to restart needs the developer password")
	// ErrRestartCannotHear is a service whose database role may not read
	// panel_settings: it would never see the request.
	ErrRestartCannotHear = errors.New("panel: that service's database role cannot read panel_settings, so it would never see the request")
	// ErrRestartNeverReported is a service with no heartbeat row: it has
	// never run under a build that reports, so nothing could tell whether
	// a request was honoured, and nothing listening could know its own
	// start to compare with.
	ErrRestartNeverReported = errors.New("panel: that service has never reported, so it cannot be asked")
	// ErrRestartTooSoon is a second request inside RestartSpacing.
	ErrRestartTooSoon = errors.New("panel: that service was asked to restart less than a minute ago")
)

// RestartRequest is a request that was written.
type RestartRequest struct {
	Service string
	// At is the request, in the database's clock - the value of the row.
	At time.Time
	// AuditID is the entry written with it, so the operation record can
	// point at it.
	AuditID int64
}

// RequestRestart asks one service to restart.
//
// The order is the order of what the checks reveal. Entitlement first: a
// viewer is refused on who they are, so nothing they typed is read. The
// password second, before anything about the services is said - which
// ones exist, which can hear - so that a person without it learns
// nothing from the refusal. Then the closed list, whether the role can
// hear, whether it has ever reported, and the spacing, inside one
// transaction with the row and its audit entry: a request whose record
// cannot be written does not happen.
func (s *Store) RequestRestart(ctx context.Context, a Access, auth devgate.Authorization,
	service string) (RestartRequest, error) {

	if err := mayOperateDeployment(a, "restart"); err != nil {
		return RestartRequest{}, err
	}
	if !auth.Authorizes(RestartGateAction) {
		return RestartRequest{}, ErrRestartNeedsPassword
	}
	// The form offers a select, and a select is a suggestion: the value
	// arrives as whatever string the browser - or anybody - sent.
	if !slices.Contains(relupdate.HealthServices, service) {
		return RestartRequest{}, fmt.Errorf("%w: %q", ErrRestartUnknownService, service)
	}

	out := RestartRequest{Service: service}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		canHear, reported, err := restartAskable(ctx, tx, service)
		if err != nil {
			return err
		}
		if !canHear {
			return ErrRestartCannotHear
		}
		if !reported {
			return ErrRestartNeverReported
		}

		// The spacing is decided by the statement that writes, so two
		// presses at once cannot both find the last request old enough:
		// the second waits on the first's row and then re-evaluates the
		// WHERE against it. No row back means "too soon".
		err = tx.QueryRow(ctx, `
			INSERT INTO panel_settings (scope, site_id, key, value, updated_by)
			VALUES ('global', '', $1, to_jsonb(now()), $2)
			ON CONFLICT (scope, site_id, key) DO UPDATE
			   SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by
			 WHERE panel_settings.updated_at <= now() - make_interval(secs => $3)
			RETURNING (value #>> '{}')::timestamptz`,
			restartreq.Key(service), actorIDOf(a.Principal), RestartSpacing.Seconds()).Scan(&out.At)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRestartTooSoon
		}
		if err != nil {
			return fmt.Errorf("panel: write the restart request: %w", err)
		}

		entry := AuditEntry{
			ActorKind:  a.Principal.Kind,
			ActorID:    actorIDOf(a.Principal),
			ActorLabel: a.Principal.Label,
			Action:     ActionRestartRequested,
			Target:     service,
			Detail:     map[string]any{"requested_at": out.At},
		}
		out.AuditID, err = recordIn(ctx, tx, entry)
		return err
	})
	if err != nil {
		return RestartRequest{}, err
	}
	return out, nil
}

// restartAskable asks the database whether a service can hear a request
// and has ever reported.
//
// The privilege is asked about the role, not read through it, and through
// to_regrole and to_regclass so that a role or a table this database does
// not have is a plain "no" rather than an error: the service's own
// watcher asks the same question the same way about itself.
func restartAskable(ctx context.Context, q rowQuerier, service string) (canHear, reported bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT coalesce(has_table_privilege(to_regrole($1)::oid,
		                                    to_regclass('panel_settings')::oid, 'SELECT'), false),
		       EXISTS (SELECT 1 FROM service_heartbeat WHERE service = $1)`,
		service).Scan(&canHear, &reported)
	if err != nil {
		return false, false, fmt.Errorf("panel: ask whether %s can be restarted: %w", service, err)
	}
	return canHear, reported, nil
}

// RestartState is what the page says about one service's last request.
type RestartState string

const (
	// RestartCannotHear: the role may not read panel_settings.
	RestartCannotHear RestartState = "duymaz"
	// RestartNeverReported: no heartbeat row at all.
	RestartNeverReported RestartState = "raporsuz"
	// RestartNone: never asked.
	RestartNone RestartState = "yok"
	// RestartDone: the process running now started at the request or
	// after it.
	RestartDone RestartState = "basladi"
	// RestartNotReporting: not honoured, and the heartbeat is stale - the
	// process is not running, or cannot reach the database, so it cannot
	// read a request either.
	RestartNotReporting RestartState = "sessiz"
	// RestartPending: not honoured yet, and younger than RestartSpacing.
	RestartPending RestartState = "bekliyor"
	// RestartIgnored: not honoured, older than RestartSpacing, and the
	// heartbeat is fresh - the process is running and did not act on it.
	// A build older than the watcher is the likeliest reason.
	RestartIgnored RestartState = "uygulanmadi"
)

// Askable reports whether a service in this state may be asked.
//
// The two refusals that do not depend on time. Spacing is not one of
// them: it is decided by the write, and a list that also guessed it
// would be a second copy that could disagree.
func (st RestartState) Askable() bool {
	return st != RestartCannotHear && st != RestartNeverReported
}

// RestartService is one service as the page draws it.
type RestartService struct {
	Service string
	State   RestartState
	// Requested is the last request, zero when there has been none.
	Requested time.Time
	// Started is the running process's start from its heartbeat, zero
	// when it has none.
	Started time.Time
}

// RestartStatus is the section, one row per service in
// relupdate.HealthServices order.
type RestartStatus struct {
	// Allowed reports whether this principal may press the button at
	// all; the password is still asked every time.
	Allowed  bool
	Services []RestartService
}

// RestartStatus reads every service's last request, whether its role can
// read one, and its heartbeat.
//
// The time every comparison is made against is the database's, read in
// the same statement as the requests: the requests and the heartbeats
// are in its clock, and a page that judged them by the panel's would be
// comparing two clocks again.
func (s *Store) RestartStatus(ctx context.Context, a Access) (RestartStatus, error) {
	out := RestartStatus{Allowed: a.Can(CapManageSettings)}

	keys := make([]string, len(relupdate.HealthServices))
	for i, service := range relupdate.HealthServices {
		keys[i] = restartreq.Key(service)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.service,
		       coalesce(has_table_privilege(to_regrole(r.service)::oid,
		                                    to_regclass('panel_settings')::oid, 'SELECT'), false),
		       (SELECT p.value FROM panel_settings p
		         WHERE p.scope = 'global' AND p.site_id = '' AND p.key = r.key),
		       now()
		  FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS r(service, key, n)
		 ORDER BY r.n`, relupdate.HealthServices, keys)
	if err != nil {
		return out, fmt.Errorf("panel: restart status: %w", err)
	}
	type asked struct {
		service   string
		canHear   bool
		requested []byte
	}
	var (
		list []asked
		now  time.Time
	)
	for rows.Next() {
		var row asked
		if err := rows.Scan(&row.service, &row.canHear, &row.requested, &now); err != nil {
			rows.Close()
			return out, fmt.Errorf("panel: restart status: %w", err)
		}
		list = append(list, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("panel: restart status: %w", err)
	}

	beats, err := heartbeat.Read(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: restart status: %w", err)
	}
	byService := make(map[string]heartbeat.Beat, len(beats))
	for _, b := range beats {
		byService[b.Service] = b
	}

	for _, row := range list {
		beat, reported := byService[row.service]
		svc := RestartService{Service: row.service}
		// Read the way the service reads it, with the service's own
		// function: a value that is not a time is no request to the
		// service, so it is none here either - rather than a cast in SQL
		// that would make the whole section unreadable over one row.
		if row.requested != nil {
			if at, err := restartreq.ParseRequest(row.requested); err == nil {
				svc.Requested = at
			}
		}
		if reported {
			svc.Started = beat.StartedAt
		}
		svc.State = restartStateOf(row.canHear, reported, beat, svc.Requested, now)
		out.Services = append(out.Services, svc)
	}
	return out, nil
}

// restartStateOf is the one place the states are decided.
//
// The order is the order of what each state can know. A service that
// cannot hear, or has never reported, has no request worth judging.
// Then the request: none, honoured, or not - and "honoured" is
// restartreq.Satisfied and nothing else, the same sentence the service's
// watcher exits on. An unhonoured request is judged by the heartbeat
// before its age: a stale heartbeat means nothing is listening, whatever
// the request's age, and saying "waiting" for a minute about a process
// that stopped reporting three minutes ago would be a minute of a page
// promising something that cannot happen.
func restartStateOf(canHear, reported bool, beat heartbeat.Beat, requested, now time.Time) RestartState {
	switch {
	case !canHear:
		return RestartCannotHear
	case !reported:
		return RestartNeverReported
	case requested.IsZero():
		return RestartNone
	case restartreq.Satisfied(requested, beat.StartedAt):
		return RestartDone
	case beat.Stale(now, heartbeat.DefaultInterval):
		return RestartNotReporting
	case now.Sub(requested) < RestartSpacing:
		return RestartPending
	default:
		return RestartIgnored
	}
}
