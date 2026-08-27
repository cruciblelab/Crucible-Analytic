package web

import (
	"context"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
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
		s.renderHealth(w, r, lang, p)
	default:
		w.Header().Set("Allow", "GET, HEAD")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
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

func (s *Server) renderHealth(w http.ResponseWriter, r *http.Request, lang *ui.Language, p panel.Principal) {
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
	data.Storage, data.StorageError = s.healthStorage(ctx, lang)
	data.API = s.healthAPI(ctx)

	page := s.page(r, lang, panel.Access{Principal: p}, "saglik", lang.T("saglik.baslik"))
	page.Data = data
	s.Renderer.Render(w, r, http.StatusOK, "saglik", page)
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
