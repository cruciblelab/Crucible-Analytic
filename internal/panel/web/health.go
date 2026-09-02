package web

import (
	"context"
	"errors"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"net/http"
	"strconv"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
)

// The system health page.
//
// # What it is for
//
// The highest-value thing for "log into the VDS less often" is not
// repair, it is knowing what is broken before somebody rings. This page
// is that, and its whole design is one rule:
//
//	every section fails on its own.
//
// A health page whose sections all go dark together is a health page
// that says nothing at the moment it is needed. So the services come
// from a database table, the storage facts from a second query, and the
// read API from an HTTP request - three sources, three independent
// failures, and a section that could not be filled says so where it
// would have been rather than taking the page with it.
//
// # Who sees it
//
// An owner, and a developer.
//
// Different from the mail page, deliberately. There the developer is
// refused because whoever controls outgoing mail receives every
// password-reset link - configuring it is close to becoming any user.
// Reading a health page grants nothing: it is a build number, a byte
// count and an uptime. It is also the developer's own diagnostic tool,
// and a support call that starts with "I cannot see anything" is the
// call this page exists to prevent.
//
// # What is not on it
//
// Any number derived from a visitor. Not one. Traffic counts live behind
// the read-only API and the per-site pages, and putting "events today"
// here would give the panel's role a route to the analytics it is
// specifically not allowed to have - through a page nobody would think
// to check, because the panel is supposed to read this table.

// HealthPath is the system health page.
const HealthPath = "/saglik"

// The two buttons this page carries, named in the form rather than
// inferred from which fields arrived.
//
// A handler that guesses what the client meant is a handler that can be
// made to guess wrong, and this page's two actions have different
// entitlements behind them: one is entitlement-only, the other can be
// locked to the developer password.
const (
	eylemYukselt      = "yukselt"
	eylemKaynakYenile = "kaynak_yenile"
)

// healthPage is Data for the template.
type healthPage struct {
	// Services is one row per service that has ever written a heartbeat.
	Services []healthService
	// ServicesError is filled when the heartbeat table could not be
	// read - a deployment that has not applied the schema, most likely.
	ServicesError string
	// NoServices means the table is there and empty: every service is
	// running a build from before the heartbeat existed, or none of them
	// is running.
	NoServices bool

	Storage      []healthStorage
	StorageError string

	API healthAPI

	// Panel is this process, which never writes a heartbeat row - it has
	// no reason to tell itself it is alive. Shown so the page reports
	// four builds rather than three and a gap.
	Panel healthService

	Schema      healthSchema
	SchemaError string

	Upgrade      upgradeSection
	UpgradeError string

	// The M3 refresh button and, beneath it, the M2 fetch log. Its own
	// section with its own error field, like every other part of this
	// page: an unreadable fetch log must not take the storage figures
	// down with it.
	RangeRefresh      rangeRefreshSection
	RangeRefreshError string
}

// healthSchema is what the database says its schema is, next to what
// this build expects.
//
// The pair is the point. A version on its own is a number somebody
// typed; a version beside the one the running code wants is the answer
// to "why did the collector stop writing" - which, measured, is a
// process that starts, passes its ping, and loses every row it is
// handed. See internal/schemaver.
type healthSchema struct {
	// Installed is what the database holds, "-" when nothing is
	// recorded.
	Installed string
	// Expected is what this build was compiled against.
	Expected string

	// Matches is the only field that should normally be true, and the
	// three below say what kind of mismatch it is when it is not.
	Matches bool
	// Ahead means the binary wants a newer schema than is installed:
	// the direction that loses rows.
	Ahead bool
	// Behind means the database is newer than the binary. Safe - an old
	// binary writes through an added column without noticing - but it
	// means somebody rolled a binary back and left the schema.
	Behind bool
	// Unrecorded means this database predates schema versioning. Not a
	// fault, and not a match either.
	Unrecorded bool

	AppliedBy   string
	Fingerprint string
}

// healthService is one service as the page draws it.
type healthService struct {
	// Name is the database role, which is also the identity the
	// row-level policy checks. The template turns it into a label.
	Name    string
	Version string
	// Stale means the last beat is older than three intervals. Not
	// "down": this page cannot know that, and a page that says DOWN
	// about a service having a slow minute is a page somebody stops
	// believing.
	Stale bool
	// Missing means this service has no row at all, which is a different
	// fact from a stale one and wants a different sentence.
	Missing bool

	LastBeat time.Time
	Uptime   time.Duration

	// Profile is the resource profile this service reports, already
	// turned into the name a person reads. Empty for the services that
	// have none and for a collector too old to report one; the template
	// draws a dash there rather than inventing a value.
	//
	// The label comes from internal/profile rather than from the message
	// table, because that package is where the profiles are defined and
	// a second list of their names would be a second thing to update.
	// An id this build does not know is shown raw: a newer collector
	// against an older panel should say something true rather than
	// nothing at all.
	Profile string

	Counters []healthCounter

	LastError   string
	LastErrorAt time.Time
}

// healthCounter is one counter with its label already resolved.
type healthCounter struct {
	Label string
	Value int64
}

// healthStorage is one table's size and shape.
type healthStorage struct {
	Table string
	// Label is the table's name in the reader's language.
	Label string
	Bytes int64
	// Missing means the table is not in the database. Its own flag
	// rather than a zero size: "not installed" and "installed and empty"
	// send an operator to two different places.
	Missing    bool
	Hypertable bool
	Chunks     int64
	// Retention is the age at which rows are dropped, zero when nothing
	// drops them.
	Retention time.Duration
}

// healthAPI is whether the read-only API answered.
type healthAPI struct {
	// Configured is whether an address is set at all. Unset is a
	// supported deployment, not a fault.
	Configured bool
	Reachable  bool
	Detail     string
	// Took is how long the request took, which is the difference between
	// "answering" and "answering in four seconds".
	Took time.Duration
}

// healthCounterOrder is the order counters are drawn in, and the closed
// set the labels are looked up from.
//
// Ordered by what an operator reads first. Dropped is not last: it is
// the number that means data was lost, and a page that buries it under
// three larger numbers has buried the only line that needed reading.
var healthCounterOrder = []string{
	heartbeat.CounterDropped,
	heartbeat.CounterWritten,
	heartbeat.CounterAccepted,
	heartbeat.CounterRejected,
	heartbeat.CounterErrors,
}

// healthHandler serves the page.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if !s.haveStore(w, r, lang) {
		return
	}
	p, ok := s.requireHealthReader(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderHealth(w, r, lang, p, pressed{})
	case http.MethodPost:
		// The upgrade button. The page was read-only until L3, which is
		// why healthHandler used to sit in the deliberately-unguarded
		// list in limits_integration_test.go - and why it has to move
		// out of it now.
		if !s.acceptPost(w, r, lang) {
			return
		}
		access := s.healthAccess(r.Context(), p)
		// Two buttons on one page now, so the form says which. Dispatched
		// on an explicit value rather than on which field happens to be
		// present: a handler that guesses what the client meant is a
		// handler that can be made to guess wrong.
		var posted pressed
		switch r.FormValue("eylem") {
		case eylemKaynakYenile:
			section, sectionErr := s.rangeRefreshPost(w, r, lang, access)
			posted.refresh, posted.refreshErr = &section, sectionErr
		case eylemYukselt:
			section, sectionErr := s.upgradePost(w, r, lang, access)
			posted.upgrade, posted.upgradeErr = &section, sectionErr
		default:
			s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
			return
		}
		s.renderHealth(w, r, lang, p, posted)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

// healthAccess is what this principal may do, for the upgrade section.
//
// The page's own entry check (requireHealthReader) answers "may you read
// this", which is a different question from "may you press that" - and
// conflating them is how a read-only page grows a button somebody was
// never meant to have.
func (s *Server) healthAccess(ctx context.Context, p panel.Principal) panel.Access {
	access := panel.Access{Principal: p}
	if p.Superadmin || p.Kind == panel.PrincipalDeveloper {
		return access
	}
	// An owner of any site is the customer this deployment belongs to.
	// The schema is deployment-wide, so there is no per-site answer to
	// give here.
	if s.ownsAnySite(ctx, p) {
		access.Role = panel.RoleOwner
		access.Member = true
	}
	return access
}

// requireHealthReader resolves somebody entitled to read the page.
//
// An owner or a developer. The developer is allowed here and refused on
// the mail page, and the difference is what the page hands over: mail
// configuration is close to becoming any user, and a byte count is a
// byte count.
func (s *Server) requireHealthReader(w http.ResponseWriter, r *http.Request) (panel.Principal, bool) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return panel.Principal{}, false
	}
	if p.Kind == panel.PrincipalDeveloper {
		return p, true
	}
	if s.ownsAnySite(r.Context(), p) {
		return p, true
	}
	s.Renderer.ErrorIn(w, r, http.StatusForbidden, s.language(r))
	return panel.Principal{}, false
}

// pressed is what a POST has already decided, so the render below does
// not read it a second time.
//
// A struct of pointers rather than a second render function: this file
// already warns that a handler with two render sites grows a third, and
// a page with two buttons would have made it three today.
type pressed struct {
	upgrade    *upgradeSection
	upgradeErr string
	refresh    *rangeRefreshSection
	refreshErr string
}

func (s *Server) renderHealth(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, posted pressed) {
	ctx := r.Context()
	now := time.Now()

	data := healthPage{
		Panel: healthService{
			Name:    "panel_user",
			Version: buildinfo.Version(s.Renderer.Version),
		},
	}

	// Three sources, gathered independently. Each failure is written
	// into its own field and none of them returns early - which is the
	// page's entire reason for existing.
	data.Services, data.ServicesError, data.NoServices = s.healthServices(ctx, lang, now)
	data.Schema, data.SchemaError = s.healthSchema(ctx, lang)
	data.Storage, data.StorageError = s.healthStorage(ctx, lang)
	data.API = s.healthAPI(ctx)

	// Read fresh unless the press just built it, because the answer a
	// pressed section carries is about the press rather than about the
	// state before it.
	//
	// Per section, not per request: pressing refresh must still show the
	// current upgrade state, and the version before this one rebuilt only
	// the section that was pressed while leaving the other one empty -
	// which drew a page with half of it missing.
	access := s.healthAccess(ctx, p)
	if posted.upgrade != nil {
		data.Upgrade, data.UpgradeError = *posted.upgrade, posted.upgradeErr
	} else {
		data.Upgrade, data.UpgradeError = s.upgradeStatusFor(r, lang, access)
	}
	if posted.refresh != nil {
		data.RangeRefresh, data.RangeRefreshError = *posted.refresh, posted.refreshErr
	} else {
		data.RangeRefresh, data.RangeRefreshError = s.rangeRefreshStatusFor(r, lang, access)
	}

	page := s.page(r, lang, panel.Access{Principal: p}, "saglik", lang.T("saglik.baslik"))
	page.Data = data
	s.Renderer.Render(w, r, http.StatusOK, "saglik", page)
}

// healthSchema asks the database what schema it carries.
//
// Its own gatherer with its own error field, like every other section
// here: this page exists because an operator needs the parts that work
// even when one part does not, and a schema read that fails must not
// take the heartbeat table down with it.
func (s *Server) healthSchema(ctx context.Context, lang *ui.Language) (healthSchema, string) {
	out := healthSchema{Expected: strconv.Itoa(schemaver.Version)}

	st, err := schemaver.Read(ctx, s.Store.Pool())
	switch {
	case errors.Is(err, schemaver.ErrNoTable):
		// An installation made before schema versioning existed. Said
		// plainly rather than as an error, because it is not one - it
		// is what every deployment older than this feature looks like,
		// and the fix is a line in the release notes, not a repair.
		out.Unrecorded = true
		out.Installed = "-"
		return out, ""
	case err != nil:
		s.logger().Error("panel: reading the schema version", "err", err)
		return out, lang.T("saglik.sema.okunamadi")
	}

	if !st.Recorded {
		out.Unrecorded = true
		out.Installed = "-"
		return out, ""
	}

	out.Installed = strconv.Itoa(st.Version)
	out.AppliedBy = st.AppliedBy
	out.Fingerprint = st.Fingerprint
	out.Matches = st.Matches()
	out.Ahead = st.Ahead()
	out.Behind = st.Behind()

	// Matched on the fingerprint, reported by the version. A database
	// whose number agrees and whose fingerprint does not is the case
	// worth naming: same version, different schema, which is what a
	// half-applied upgrade leaves behind.
	if !out.Matches && !out.Ahead && !out.Behind {
		out.Ahead = true
	}
	return out, ""
}

// healthServices reads the heartbeat table.
func (s *Server) healthServices(ctx context.Context, lang *ui.Language, now time.Time) ([]healthService, string, bool) {
	beats, err := heartbeat.Read(ctx, s.Store.Pool())
	if err != nil {
		s.logger().Error("panel: reading service heartbeats", "err", err)
		return nil, lang.T("saglik.servis.okunamadi"), false
	}
	if len(beats) == 0 {
		return nil, "", true
	}

	out := make([]healthService, 0, len(beats))
	for _, b := range beats {
		row := healthService{
			Name:        b.Service,
			Version:     b.Version,
			Stale:       b.Stale(now, heartbeat.DefaultInterval),
			LastBeat:    b.BeatAt,
			Uptime:      b.Uptime(),
			LastError:   b.LastError,
			LastErrorAt: b.LastErrorAt,
			Profile:     profileLabel(b.Profile),
		}
		// Only counters with a label, in a fixed order. A counter a
		// service invented and nobody has words for would otherwise
		// render as a raw identifier on an operator's screen; the test
		// in health_test.go keeps the two lists together.
		for _, key := range healthCounterOrder {
			value, present := b.Counters[key]
			if !present {
				continue
			}
			row.Counters = append(row.Counters, healthCounter{
				Label: lang.T("saglik.sayac." + key),
				Value: value,
			})
		}
		out = append(out, row)
	}
	return out, "", false
}

// healthStorage reads what the panel may know about the tables.
func (s *Server) healthStorage(ctx context.Context, lang *ui.Language) ([]healthStorage, string) {
	facts, err := s.Store.StorageFacts(ctx)
	if err != nil {
		s.logger().Error("panel: reading storage facts", "err", err)
		return nil, lang.T("saglik.depolama.okunamadi")
	}
	out := make([]healthStorage, 0, len(facts))
	for _, f := range facts {
		out = append(out, healthStorage{
			Table:      f.Table,
			Label:      lang.T("saglik.tablo." + f.Table),
			Bytes:      f.Bytes,
			Missing:    f.Bytes == 0 && !f.Hypertable,
			Hypertable: f.Hypertable,
			Chunks:     f.Chunks,
			Retention:  f.RetentionAfter,
		})
	}
	return out, ""
}

// healthAPI asks the read-only API whether it is there.
//
// Its own short deadline. The page must render while the API is wedged,
// and "wedged" is the case a generous timeout turns into a page that
// hangs - which is indistinguishable, from a browser, from a panel that
// is down too.
func (s *Server) healthAPI(ctx context.Context) healthAPI {
	out := healthAPI{Configured: s.Analytics.Configured()}
	if !out.Configured {
		return out
	}

	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	started := time.Now()
	// KnownSites is the cheapest authenticated call this client has, and
	// it exercises the whole path the dashboard uses: the address, the
	// token, and the API's own database connection. A bare /healthz would
	// prove the process is listening and nothing about whether the panel
	// can actually read through it - which is the question.
	_, _, err := s.Analytics.KnownSites(ctx, true, false)
	out.Took = time.Since(started)
	if err != nil {
		out.Detail = err.Error()
		return out
	}
	out.Reachable = true
	return out
}

// profileLabel turns a reported profile id into the name a person reads.
//
// An empty id stays empty: three of the four services have no profile,
// and a placeholder would look like one they had.
func profileLabel(id string) string {
	if id == "" {
		return ""
	}
	if p, ok := profile.ByID(id); ok {
		return p.Label
	}
	// Unknown, which means a service newer than this panel. Its own id
	// is the most truthful thing available, and more useful than a blank
	// cell to whoever is working out why the two disagree.
	return id
}
