package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The developer wizard.
//
// It is reached before anybody owns the deployment, through a one-time
// link minted on the server itself, and it covers the technical ground:
// the database, the sites, what the config files say, how long data is
// kept, and finally whether the whole thing actually works.
//
// Two rules shape every step below.
//
// First, **the wizard verifies more than it configures.** Most of what
// a deployment needs - the database roles, the schema, the TLS
// certificate, the collector's backend - the panel cannot set and
// should not be able to. So those steps read the real state and report
// it, rather than presenting a field that writes nothing. A wizard that
// pretends to configure something it cannot is worse than one that says
// where to go.
//
// Second, **each step commits what it changes, immediately.** There is
// no draft accumulating in the session and no "finish" that applies
// everything at once. Somebody who gets halfway and closes the tab has
// left a half-configured deployment, which is true and visible, rather
// than a deployment that looks untouched while a session somewhere
// holds their answers.

// SetupPathPrefix is where the wizard lives.
const SetupPathPrefix = "/kurulum/"

// DevAccessPathPrefix is where a one-time developer link is redeemed.
const DevAccessPathPrefix = "/gelistirici/"

// wizardStep is one page of the wizard.
//
// A closed, ordered list rather than a map: the order is the product -
// an installer works down it - and a step reachable only by typing its
// name would be a step nobody runs.
type wizardStep struct {
	// ID is the URL segment and the message-key suffix.
	ID string
	// Writes reports whether this step changes anything. Steps that only
	// verify say so on the page, because "I filled this in and nothing
	// happened" is the complaint a read-only step earns if it looks
	// like a form.
	Writes bool
}

var wizardSteps = []wizardStep{
	{ID: "baslangic"},
	{ID: "veritabani"},
	{ID: "siteler", Writes: true},
	{ID: "toplama"},
	{ID: "saklama", Writes: true},
	{ID: "kontrol"},
}

func stepIndex(id string) int {
	for i, step := range wizardSteps {
		if step.ID == id {
			return i
		}
	}
	return -1
}

// stepView is what the progress list renders from.
type stepView struct {
	ID      string
	Title   string
	URL     string
	Number  int
	Current bool
	Done    bool
}

// setupPage is the Data every wizard step carries, plus its own fields.
type setupPage struct {
	Steps    []stepView
	Step     string
	Number   int
	Total    int
	NextURL  string
	BackURL  string
	Last     bool
	ReadOnly bool

	// Sites is the beacon allowlist, for the step that edits it.
	Sites []string
	// SitesRaw is what the form field shows.
	SitesRaw string

	// Config is the read-only view of the config files.
	ConfigNotice   string
	ConfigSettings []panel.ConfigFileSetting

	// Checks are preflight results, on the database and final steps.
	Checks   []panel.CheckResult
	Blocking []panel.CheckResult
	Complete bool
	Ran      bool

	// Manual and Unchecked are the steps the panel can never do.
	Manual    []panel.ManualStep
	Unchecked []panel.ManualStep

	// Retention is one row per editable retention value.
	Retention []retentionRow
	// Prompt is the developer password prompt, when one is needed.
	Prompt panel.DeveloperPasswordPrompt
	// Message is the outcome of the last save.
	Message string
	// Failed marks that message as a failure rather than a success.
	Failed bool
}

// setupHandler routes every /kurulum/ request.
func (s *Server) setupHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)

	access, ok := s.developerAccess(w, r, lang)
	if !ok {
		return
	}

	id := strings.Trim(strings.TrimPrefix(r.URL.Path, SetupPathPrefix), "/")
	if id == "" {
		http.Redirect(w, r, SetupPathPrefix+wizardSteps[0].ID, http.StatusSeeOther)
		return
	}
	index := stepIndex(id)
	if index < 0 {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderStep(w, r, lang, access, index, setupPage{})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			// 419 rather than 403: the fix is "reload and try again",
			// not "you may not". A wizard left open while somebody went
			// to run a command on the server is the normal way to get
			// here.
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.saveStep(w, r, lang, access, index)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

// statusCSRFExpired mirrors the renderer's code for a stale form. Not
// in the RFC, and deliberately distinct from 403 - see the renderer.
const statusCSRFExpired = 419

// developerAccess resolves who is asking and refuses anybody who is not
// here through a developer link.
//
// The refusal is a page rather than a redirect to a login form, because
// there may be no accounts at all: sending somebody to sign in to a
// deployment with no users is a loop.
func (s *Server) developerAccess(w http.ResponseWriter, r *http.Request, lang *ui.Language) (panel.Access, bool) {
	principal, err := s.Sessions.Principal(r.Context())
	if err != nil || !principal.Superadmin {
		s.renderSetupNeeded(w, r, lang, http.StatusForbidden)
		return panel.Access{}, false
	}
	return panel.Access{Principal: principal}, true
}

// renderSetupNeeded is the page somebody lands on with no developer
// session: what this deployment is waiting for, and the exact command
// that produces a link.
//
// Printing the command is the whole point. The alternative - "ask your
// administrator" - is useless when the person reading it *is* the
// administrator, standing at a shell, which is the only situation in
// which this page is ever seen.
func (s *Server) renderSetupNeeded(w http.ResponseWriter, r *http.Request, lang *ui.Language, status int) {
	users, err := s.Store.CountUsers(r.Context())
	if err != nil {
		s.logger().Error("panel: count users", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	s.Renderer.Render(w, r, status, "kurulum_gerekli", &ui.Page{
		L:       lang,
		Title:   lang.T("kurulum.gerekli.baslik"),
		Heading: lang.T("kurulum.gerekli.baslik"),
		F:       ui.NewFormatter(lang, s.Zone),
		Data: struct {
			FirstRun bool
			Command  string
		}{
			FirstRun: users == 0,
			Command:  "panel -config " + s.ConfigPath + " -dev-link",
		},
	})
}

func (s *Server) renderStep(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access, index int, data setupPage) {

	step := wizardSteps[index]
	data.Step = step.ID
	data.Number = index + 1
	data.Total = len(wizardSteps)
	data.Last = index == len(wizardSteps)-1
	data.ReadOnly = !step.Writes
	if index > 0 {
		data.BackURL = SetupPathPrefix + wizardSteps[index-1].ID
	}
	if index < len(wizardSteps)-1 {
		data.NextURL = SetupPathPrefix + wizardSteps[index+1].ID
	}
	for i, other := range wizardSteps {
		data.Steps = append(data.Steps, stepView{
			ID:      other.ID,
			Title:   lang.T("kurulum.adim." + other.ID + ".baslik"),
			URL:     SetupPathPrefix + other.ID,
			Number:  i + 1,
			Current: i == index,
			Done:    i < index,
		})
	}

	if err := s.loadStep(r.Context(), lang, access, step.ID, &data); err != nil {
		s.logger().Error("panel: load wizard step", "step", step.ID, "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	s.Renderer.Render(w, r, http.StatusOK, "kurulum_"+step.ID, &ui.Page{
		L:       lang,
		Title:   lang.T("kurulum.adim." + step.ID + ".baslik"),
		Heading: lang.T("kurulum.adim." + step.ID + ".baslik"),
		F:       ui.NewFormatter(lang, s.Zone),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		User: ui.UserView{
			Label:         access.Principal.Label,
			Operator:      true,
			DeveloperMode: access.Principal.DeveloperMode,
		},
		Notices: s.setupNotices(lang, data),
		Data:    data,
	})
}

// setupNotices is the banner above every wizard page.
func (s *Server) setupNotices(lang *ui.Language, data setupPage) []ui.Notice {
	notices := []ui.Notice{{
		Level: ui.NoticeWarn,
		Title: lang.T("kurulum.uyari.baslik"),
		Body:  lang.T("kurulum.uyari.govde"),
	}}
	if data.Message != "" {
		level := ui.NoticeInfo
		if data.Failed {
			level = ui.NoticeError
		}
		notices = append(notices, ui.Notice{Level: level, Body: data.Message})
	}
	return notices
}

// loadStep fills in whatever this step needs to render.
func (s *Server) loadStep(ctx context.Context, lang *ui.Language, access panel.Access, id string, data *setupPage) error {
	switch id {
	case "veritabani":
		results := s.Store.RunPreflight(ctx, s.preflightConfig())
		data.Checks = databaseChecks(results)
		data.Complete, data.Blocking = panel.PreflightComplete(data.Checks)
		data.Ran = true

	case "siteler":
		value, err := s.Store.GetSetting(ctx, panel.KeyBeaconSites, "")
		if err != nil {
			return err
		}
		data.Sites = toStringList(value)
		data.SitesRaw = strings.Join(data.Sites, "\n")

	case "toplama":
		data.ConfigNotice = panel.ConfigFileNotice
		data.ConfigSettings = panel.ConfigFileSettings(s.ConfigFileValues)

	case "saklama":
		rows, err := s.retentionRows(ctx, access)
		if err != nil {
			return err
		}
		data.Retention = rows
		data.Prompt = panel.PromptFor(access, s.Gate != nil && s.Gate.Configured(), retentionKeys...)
		if !anySiteScoped(rows) {
			// Analytics retention is per site, so with no sites there is
			// nothing to set it on. Saying that beats rendering a field
			// whose save would be refused by the store.
			data.Message = lang.T("kurulum.saklama.site_yok")
		}

	case "kontrol":
		data.Manual = panel.ManualSteps()
		data.Unchecked = panel.UncheckedSteps()
		if data.Ran {
			data.Complete, data.Blocking = panel.PreflightComplete(data.Checks)
		}
	}
	return nil
}

// retentionKeys are the settings the retention step touches. Both carry
// legal weight, so both are behind the developer password - which is
// exactly why this step is in the developer's wizard and not the
// owner's.
var retentionKeys = []panel.Key{
	panel.KeyAnalyticsRetentionDays,
	panel.KeyLogRetentionDays,
}

// retentionRow is one editable value on the retention step.
//
// The two keys have different scopes and the page has to respect that
// rather than average over it: log retention is one number for the
// deployment, and analytics retention belongs to a site. Writing the
// second one globally is not a simplification, it is a value the store
// refuses - which is how this was found.
type retentionRow struct {
	// Site is empty for a deployment-wide value.
	Site string
	// Field is the form input's name. Site-scoped rows carry the site in
	// it, so two sites cannot collide in one POST.
	Field string
	View  panel.SettingView
}

// SiteScoped reports whether this row belongs to one site.
func (r retentionRow) SiteScoped() bool { return r.Site != "" }

func anySiteScoped(rows []retentionRow) bool {
	for _, row := range rows {
		if row.SiteScoped() {
			return true
		}
	}
	return false
}

// retentionFieldName is the form name for one row. The separator is a
// colon because a site id cannot contain one - the settings registry
// bounds it to letters, digits, underscore and dash - so the two halves
// can always be split apart again.
func retentionFieldName(key panel.Key, site string) string {
	if site == "" {
		return string(key)
	}
	return string(key) + ":" + site
}

// retentionRows builds the page: the deployment-wide log retention, and
// one analytics retention per configured site.
func (s *Server) retentionRows(ctx context.Context, access panel.Access) ([]retentionRow, error) {
	global, err := s.Store.SettingsView(ctx, access, "")
	if err != nil {
		return nil, err
	}
	rows := []retentionRow{}
	for _, view := range global {
		if view.Definition.Key == panel.KeyLogRetentionDays {
			rows = append(rows, retentionRow{
				Field: retentionFieldName(view.Definition.Key, ""),
				View:  view,
			})
		}
	}

	value, err := s.Store.GetSetting(ctx, panel.KeyBeaconSites, "")
	if err != nil {
		return nil, err
	}
	for _, site := range toStringList(value) {
		views, err := s.Store.SettingsView(ctx, access, site)
		if err != nil {
			return nil, err
		}
		for _, view := range views {
			if view.Definition.Key == panel.KeyAnalyticsRetentionDays {
				rows = append(rows, retentionRow{
					Site:  site,
					Field: retentionFieldName(view.Definition.Key, site),
					View:  view,
				})
			}
		}
	}
	return rows, nil
}

// databaseChecks narrows the preflight to the ones this step is about.
//
// The full list belongs at the end, where it is the handover. Showing
// all of it here as well would train the installer to scroll past it
// the second time.
func databaseChecks(all []panel.CheckResult) []panel.CheckResult {
	out := make([]panel.CheckResult, 0, len(all))
	for _, check := range all {
		if strings.HasPrefix(check.ID, "schema.") ||
			strings.HasPrefix(check.ID, "grants.") ||
			strings.HasPrefix(check.ID, "roles.") ||
			strings.HasPrefix(check.ID, "retention.") {
			out = append(out, check)
		}
	}
	return out
}

// saveStep handles a POST from one step.
func (s *Server) saveStep(w http.ResponseWriter, r *http.Request, lang *ui.Language, access panel.Access, index int) {
	step := wizardSteps[index]
	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	data := setupPage{}

	switch step.ID {
	case "siteler":
		s.saveSites(r, lang, access, &data)
	case "saklama":
		s.saveRetention(r, lang, access, &data)
	case "kontrol":
		results := s.Store.RunPreflight(r.Context(), s.preflightConfig())
		data.Checks = results
		data.Ran = true
	default:
		// Nothing to save; move on rather than redisplaying.
		http.Redirect(w, r, SetupPathPrefix+wizardSteps[min(index+1, len(wizardSteps)-1)].ID, http.StatusSeeOther)
		return
	}

	s.renderStep(w, r, lang, access, index, data)
}

func (s *Server) saveSites(r *http.Request, lang *ui.Language, access panel.Access, data *setupPage) {
	sites := parseSiteList(r.PostFormValue("siteler"))
	if len(sites) == 0 {
		data.Message, data.Failed = lang.T("kurulum.siteler.bos"), true
		return
	}
	if err := s.Store.SetSetting(r.Context(), panel.KeyBeaconSites, "", sites, actorOf(access)); err != nil {
		// The bounds live in the settings registry, and its error says
		// which value it refused. Passing it through beats a generic
		// "invalid": the installer typed the value and can fix it.
		data.Message, data.Failed = err.Error(), true
		return
	}
	data.Message = lang.T("kurulum.kaydedildi")
}

func (s *Server) saveRetention(r *http.Request, lang *ui.Language, access panel.Access, data *setupPage) {
	if s.Gate == nil || !s.Gate.Configured() {
		data.Message, data.Failed = devgate.NoticeNotConfigured, true
		return
	}

	rows, err := s.retentionRows(r.Context(), access)
	if err != nil {
		data.Message, data.Failed = err.Error(), true
		return
	}

	type change struct {
		key   panel.Key
		site  string
		value int
	}
	var changes []change
	keys := map[panel.Key]bool{}
	for _, row := range rows {
		raw := strings.TrimSpace(r.PostFormValue(row.Field))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			data.Message, data.Failed = lang.Tf("kurulum.saklama.sayi_degil", raw), true
			return
		}
		// A field returned unchanged is not a change. Without this, a
		// plain "next" through the wizard would spend a developer
		// password and write an audit entry for every value on the page.
		if same(row.View.Value, n) {
			continue
		}
		changes = append(changes, change{key: row.View.Definition.Key, site: row.Site, value: n})
		keys[row.View.Definition.Key] = true
	}
	if len(changes) == 0 {
		data.Message = lang.T("kurulum.degismedi")
		return
	}

	// One verification covering every key on the form. The gate mints an
	// authorization per *action*, and an action is a key - so one
	// password answers for one key across every site the form named,
	// which is right: the person filled in one form and pressed save
	// once. Each write is still audited separately.
	wanted := make([]panel.Key, 0, len(keys))
	for key := range keys {
		wanted = append(wanted, key)
	}
	result := s.Gate.Verify(r.Context(), s.Store.GateRequest(access, r, wanted...))
	if !result.OK() {
		data.Message, data.Failed = devgate.Explain(result), true
		return
	}

	for _, c := range changes {
		auth := result.For(panel.GateAction(c.key))
		if err := s.Store.ApplySetting(r.Context(), access, c.key, c.site, c.value, auth); err != nil {
			data.Message, data.Failed = err.Error(), true
			return
		}
	}
	data.Message = lang.T("kurulum.kaydedildi")
}

// same compares a stored setting value against a number from a form.
// Stored integers arrive as int64 from the database and as int from a
// default, and a comparison that missed one of those would treat every
// unchanged field as a change.
func same(stored any, n int) bool {
	switch v := stored.(type) {
	case int:
		return v == n
	case int32:
		return int(v) == n
	case int64:
		return v == int64(n)
	case float64:
		return v == float64(n)
	}
	return false
}

func actorOf(access panel.Access) *int64 {
	if access.Principal.UserID == 0 {
		return nil
	}
	id := access.Principal.UserID
	return &id
}

// parseSiteList accepts one site per line or a comma-separated list,
// because an installer pasting from either a config file or a note
// should not have to know which this field wanted.
func parseSiteList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

func toStringList(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

// devAccessHandler redeems a one-time developer link.
func (s *Server) devAccessHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, DevAccessPathPrefix), "/")
	if token == "" {
		s.renderSetupNeeded(w, r, lang, http.StatusNotFound)
		return
	}

	grant, err := s.Store.RedeemDevAccess(r.Context(), token, peerAddr(r))
	if err != nil {
		// Every failure is the same page: unknown, expired, already
		// used, or approved for a deployment that has since acquired an
		// owner. Distinguishing them would confirm to somebody guessing
		// that a token had once been real.
		s.logger().Warn("panel: developer link refused", "err", err)
		s.renderSetupNeeded(w, r, lang, http.StatusForbidden)
		return
	}
	if err := s.Sessions.LogInDeveloper(r.Context(), grant); err != nil {
		s.logger().Error("panel: developer session", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	http.Redirect(w, r, SetupPathPrefix+wizardSteps[0].ID, http.StatusSeeOther)
}

// peerAddr is the address the redemption is recorded against.
//
// Deliberately r.RemoteAddr and never a forwarded header. The audit
// record exists to answer "where was this link used from", and a value
// the person using the link can set is not an answer. If the panel sits
// behind a proxy this records the proxy, which is at least true.
func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

func (s *Server) preflightConfig() panel.PreflightConfig {
	cfg := s.Preflight
	if cfg.DeveloperGate == nil {
		cfg.DeveloperGate = s.Gate
	}
	return cfg
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
