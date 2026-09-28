package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
)

// DiagnosticPath is the Health page's facts as one file, for the person
// supporting this deployment.
//
// Catalog #37 (ExportDiagnosticBundle): the case where somebody has to
// read everything at once, and should not need a shell to collect it.
// This is its first half. It carries what the person downloading it can
// already read on the Health page and the settings page, and nothing
// else - a download is not a way around either page's rules.
//
// Its second half is the recent WARN and ERROR lines, as the owner
// decided they may leave the machine (2026-09-28, option c): the message
// and the classified attributes, addresses masked the way the product
// stores them, the client's claim never, and no line about a site the
// person downloading may not see. See diagnosticlogs.go.
const DiagnosticPath = HealthPath + "/tani-paketi"

// diagnosticFormat names this file's shape, for a reader who meets it
// long after it was written. It changes when a field changes meaning,
// not when one is added.
const diagnosticFormat = "crucible-analytic-diagnostics/1"

// diagnosticBundle is the file.
//
// Each section carries its own error and none of them stops the others,
// the Health page's one rule: the file is wanted most when something is
// broken, so a section that cannot be read says so and the rest arrive.
type diagnosticBundle struct {
	Format       string             `json:"format"`
	GeneratedAt  time.Time          `json:"generated_at"`
	PanelVersion string             `json:"panel_version"`
	Schema       diagnosticSchema   `json:"schema"`
	Services     diagnosticServices `json:"services"`
	Storage      diagnosticStorage  `json:"storage"`
	Disk         diagnosticDisk     `json:"disk"`
	API          diagnosticAPI      `json:"api"`
	Checks       diagnosticChecks   `json:"checks"`
	Settings     diagnosticSettings `json:"settings"`
	Logs         diagnosticLogs     `json:"logs"`
	// Omitted is what this file deliberately does not carry, in the
	// file, so whoever reads it does not mistake an absence for a
	// healthy zero.
	Omitted []string `json:"omitted"`
}

type diagnosticSchema struct {
	ExpectedVersion     int    `json:"expected_version"`
	ExpectedFingerprint string `json:"expected_fingerprint"`
	// Recorded is false on a database that predates schema versioning;
	// the fields below are empty then, and that is not an error.
	Recorded    bool   `json:"recorded"`
	Version     int    `json:"version,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	AppliedBy   string `json:"applied_by,omitempty"`
	Matches     bool   `json:"matches"`
	Error       string `json:"error,omitempty"`
}

type diagnosticServices struct {
	Rows  []diagnosticService `json:"rows"`
	Error string              `json:"error,omitempty"`
}

// diagnosticService is one heartbeat row, as the table holds it.
type diagnosticService struct {
	Service     string           `json:"service"`
	Version     string           `json:"version"`
	Profile     string           `json:"profile,omitempty"`
	TokenKey    string           `json:"token_key,omitempty"`
	StartedAt   time.Time        `json:"started_at"`
	BeatAt      time.Time        `json:"beat_at"`
	AgeSeconds  int64            `json:"age_seconds"`
	Counters    map[string]int64 `json:"counters"`
	LastError   string           `json:"last_error,omitempty"`
	LastErrorAt time.Time        `json:"last_error_at,omitzero"`
}

type diagnosticStorage struct {
	Tables []diagnosticTable `json:"tables"`
	Error  string            `json:"error,omitempty"`
}

type diagnosticTable struct {
	Table            string `json:"table"`
	Bytes            int64  `json:"bytes"`
	Hypertable       bool   `json:"hypertable"`
	Chunks           int64  `json:"chunks"`
	RetentionSeconds int64  `json:"retention_seconds,omitempty"`
}

type diagnosticDisk struct {
	Container           bool                   `json:"container"`
	DatabaseLocal       bool                   `json:"database_local"`
	DatabaseKnown       bool                   `json:"database_known"`
	DatabaseBytes       int64                  `json:"database_bytes"`
	UnplacedBackupBytes int64                  `json:"unplaced_backup_bytes"`
	Filesystems         []diagnosticFilesystem `json:"filesystems"`
	Error               string                 `json:"error,omitempty"`
}

type diagnosticFilesystem struct {
	Directories   []string `json:"directories"`
	TotalBytes    int64    `json:"total_bytes"`
	UsedBytes     int64    `json:"used_bytes"`
	AvailBytes    int64    `json:"avail_bytes"`
	ReservedBytes int64    `json:"reserved_bytes"`
	BackupBytes   int64    `json:"backup_bytes"`
	AtRisk        bool     `json:"at_risk"`
	Error         string   `json:"error,omitempty"`
}

type diagnosticAPI struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	TookMillis int64  `json:"took_ms"`
	Detail     string `json:"detail,omitempty"`
}

type diagnosticChecks struct {
	// Results is every check, the passed ones included: "this passed"
	// is information a reader cannot get from a list of failures.
	Results []preflight.CheckResult `json:"results"`
	Error   string                  `json:"error,omitempty"`
}

type diagnosticSettings struct {
	Values []diagnosticSetting `json:"values"`
	Error  string              `json:"error,omitempty"`
}

type diagnosticSetting struct {
	Key            panel.Key `json:"key"`
	Value          any       `json:"value"`
	Source         string    `json:"source"`
	Live           bool      `json:"live"`
	Developer      bool      `json:"developer,omitempty"`
	ConfigFileOnly bool      `json:"config_file_only,omitempty"`
}

// diagnosticLogs is the recent WARN and ERROR lines.
type diagnosticLogs struct {
	Since     time.Time `json:"since"`
	Limit     int       `json:"limit"`
	Truncated bool      `json:"truncated"`
	// Scope is whose lines these are: "all" for the operator, "owned"
	// for an owner - lines about no site, and about the sites they own.
	Scope string              `json:"scope"`
	Lines []diagnosticLogLine `json:"lines"`
	Error string              `json:"error,omitempty"`
}

// diagnosticLogLine is one line, as it may leave the machine.
type diagnosticLogLine struct {
	At       time.Time         `json:"at"`
	Service  string            `json:"service"`
	Level    string            `json:"level"`
	Category string            `json:"category,omitempty"`
	Message  string            `json:"message"`
	Site     string            `json:"site,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	// Withheld names the attributes the line had and this file does not
	// carry, so a reader can tell "not logged" from "not sent".
	Withheld []string `json:"withheld,omitempty"`
}

// diagnosticHandler serves the file to whoever may read the Health page.
func (s *Server) diagnosticHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if !s.haveStore(w, r, lang) {
		return
	}
	p, ok := s.requireHealthReader(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
		return
	}

	now := time.Now().UTC()
	body, err := json.MarshalIndent(s.buildDiagnostic(r.Context(), lang, p, now), "", "  ")
	if err != nil {
		s.logger().Error("panel: encoding the diagnostic file", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="crucible-tani-`+now.Format("20060102-150405")+`Z.json"`)
	// It describes this deployment in detail; no cache between here and
	// the browser should keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(body, '\n'))
}

// buildDiagnostic gathers the file, each section on its own, for the
// person downloading it - whose sites decide which log lines come.
func (s *Server) buildDiagnostic(ctx context.Context, lang *ui.Language, p panel.Principal,
	now time.Time) diagnosticBundle {

	out := diagnosticBundle{
		Format:       diagnosticFormat,
		GeneratedAt:  now,
		PanelVersion: buildinfo.Version(s.Renderer.Version),
		Omitted: []string{
			lang.T("saglik.tani.disarida.gunluk"),
			lang.T("saglik.tani.disarida.site"),
			lang.T("saglik.tani.disarida.sir"),
		},
	}
	out.Schema = s.diagnosticSchema(ctx)
	out.Services = s.diagnosticServices(ctx, now)
	out.Storage = s.diagnosticStorage(ctx)
	out.Disk = diagnosticDiskFrom(s.healthDiskSection(ctx, s.database(), lang))
	out.API = diagnosticAPIFrom(s.healthAPI(ctx))
	out.Checks = s.diagnosticChecks(ctx, lang)
	out.Settings = s.diagnosticSettings(ctx)
	out.Logs = s.diagnosticLogs(ctx, p, now)
	return out
}

// diagnosticError is an error as the file carries it: the text the
// developer needs, bounded and stripped of control characters like any
// other value that leaves this process.
func diagnosticError(err error) string {
	return logging.SanitizeValue(err.Error())
}

func (s *Server) diagnosticSchema(ctx context.Context) diagnosticSchema {
	out := diagnosticSchema{
		ExpectedVersion:     schemaver.Version,
		ExpectedFingerprint: schemaver.Fingerprint,
	}
	st, err := schemaver.Read(ctx, s.Store.Pool())
	switch {
	case errors.Is(err, schemaver.ErrNoTable):
		// A deployment older than schema versioning; Recorded says so.
	case err != nil:
		out.Error = diagnosticError(err)
		return out
	}
	out.Recorded = st.Recorded
	out.Version = st.Version
	out.Fingerprint = st.Fingerprint
	out.AppliedBy = st.AppliedBy
	out.Matches = st.Matches()
	return out
}

func (s *Server) diagnosticServices(ctx context.Context, now time.Time) diagnosticServices {
	beats, err := heartbeat.Read(ctx, s.Store.Pool())
	if err != nil {
		return diagnosticServices{Rows: []diagnosticService{}, Error: diagnosticError(err)}
	}
	out := diagnosticServices{Rows: make([]diagnosticService, 0, len(beats))}
	for _, b := range beats {
		out.Rows = append(out.Rows, diagnosticService{
			Service:    b.Service,
			Version:    b.Version,
			Profile:    b.Profile,
			TokenKey:   string(b.IPTokenKey),
			StartedAt:  b.StartedAt.UTC(),
			BeatAt:     b.BeatAt.UTC(),
			AgeSeconds: int64(now.Sub(b.BeatAt) / time.Second),
			// Null when the row's counters would not parse, which
			// heartbeat.Read tolerates so the rest of the row still shows.
			// An empty object there would claim "no counters" instead.
			Counters:    b.Counters,
			LastError:   b.LastError,
			LastErrorAt: b.LastErrorAt.UTC(),
		})
	}
	return out
}

func (s *Server) diagnosticStorage(ctx context.Context) diagnosticStorage {
	facts, err := s.Store.StorageFacts(ctx)
	if err != nil {
		return diagnosticStorage{Tables: []diagnosticTable{}, Error: diagnosticError(err)}
	}
	out := diagnosticStorage{Tables: make([]diagnosticTable, 0, len(facts))}
	for _, f := range facts {
		out.Tables = append(out.Tables, diagnosticTable{
			Table:            f.Table,
			Bytes:            f.Bytes,
			Hypertable:       f.Hypertable,
			Chunks:           f.Chunks,
			RetentionSeconds: int64(f.RetentionAfter / time.Second),
		})
	}
	return out
}

// diagnosticDiskFrom takes the page's own disk section, so the file and
// the page measure the same directories the same way. The labels stay
// behind: they are words for the page, and the directories say the same
// thing.
func diagnosticDiskFrom(d healthDisk) diagnosticDisk {
	out := diagnosticDisk{
		Container:           d.Container,
		DatabaseLocal:       d.DatabaseLocal,
		DatabaseKnown:       d.DatabaseKnown,
		DatabaseBytes:       d.DatabaseBytes,
		UnplacedBackupBytes: d.UnplacedBackupBytes,
		Filesystems:         make([]diagnosticFilesystem, 0, len(d.Filesystems)),
		Error:               d.Error,
	}
	for _, fs := range d.Filesystems {
		out.Filesystems = append(out.Filesystems, diagnosticFilesystem{
			Directories:   fs.Paths,
			TotalBytes:    fs.TotalBytes,
			UsedBytes:     fs.UsedBytes,
			AvailBytes:    fs.AvailBytes,
			ReservedBytes: fs.ReservedBytes,
			BackupBytes:   fs.BackupBytes,
			AtRisk:        fs.AtRisk,
			Error:         fs.Error,
		})
	}
	return out
}

func diagnosticAPIFrom(a healthAPI) diagnosticAPI {
	return diagnosticAPI{
		Configured: a.Configured,
		Reachable:  a.Reachable,
		TookMillis: a.Took.Milliseconds(),
		Detail:     logging.SanitizeValue(a.Detail),
	}
}

func (s *Server) diagnosticChecks(ctx context.Context, lang *ui.Language) diagnosticChecks {
	if s.Preflight == nil {
		return diagnosticChecks{Results: []preflight.CheckResult{},
			Error: lang.T("saglik.tani.kontrol_yok")}
	}
	results := s.runHealthChecks(ctx)
	if len(results) == 0 {
		return diagnosticChecks{Results: []preflight.CheckResult{},
			Error: lang.T("saglik.kontroller.okunamadi")}
	}
	return diagnosticChecks{Results: results}
}

// diagnosticSettings is every deployment-wide setting and where its
// value came from.
//
// Developer settings included: the settings page groups them away for a
// customer who has not turned developer mode on, but it does not withhold
// them (see Definition.Developer), and the person this file is sent to is
// usually the developer.
func (s *Server) diagnosticSettings(ctx context.Context) diagnosticSettings {
	views, err := s.Store.SettingsView(ctx, panel.Access{}, "")
	if err != nil {
		return diagnosticSettings{Values: []diagnosticSetting{}, Error: diagnosticError(err)}
	}
	out := diagnosticSettings{Values: make([]diagnosticSetting, 0, len(views))}
	for _, v := range views {
		if v.Definition.Scope != panel.ScopeGlobal {
			continue
		}
		out.Values = append(out.Values, diagnosticSetting{
			Key:            v.Definition.Key,
			Value:          v.Value,
			Source:         v.Source,
			Live:           v.Definition.Live,
			Developer:      v.Definition.Developer,
			ConfigFileOnly: v.Definition.ConfigFileOnly,
		})
	}
	return out
}
