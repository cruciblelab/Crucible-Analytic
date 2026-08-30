package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
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
	// What the customer will actually see. Placed here because it is
	// per-site and the step before it is where the sites come from, and
	// because it is the one step in this wizard that is not a technical
	// question: it asks what this particular customer wants to look at.
	{ID: "gorunum", Writes: true},
	{ID: "toplama"},
	{ID: "saklama", Writes: true},
	{ID: "kontrol"},
	// Handover is last because it is the only step that ends the
	// installer's involvement: everything before it can be revisited,
	// and this one produces a link somebody else uses.
	{ID: "devir", Writes: true},
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

	// Visible is the per-site choice of blocks, for the step that edits
	// it.
	Visible []siteVisibility

	// Config is the read-only view of the config files.
	ConfigNotice   string
	ConfigSettings []panel.ConfigFileSetting

	// Checks are preflight results, on the database and final steps.
	Checks   []preflight.CheckResult
	Blocking []preflight.CheckResult
	Complete bool
	Ran      bool

	// Manual and Unchecked are the steps the panel can never do.
	Manual    []preflight.ManualStep
	Unchecked []preflight.ManualStep

	// Handover is the last step: the invitation that turns a finished
	// installation into an account somebody owns.
	Claims []panel.OwnerClaim
	// ClaimURL is a freshly minted link. Shown once and never again -
	// only its hash is stored - so the page has to make that plain.
	ClaimURL   string
	ClaimEmail string
	// Delivery is what happened when the panel tried to email the link.
	// A pointer so a page that never minted one draws nothing, rather
	// than drawing the zero value as "not configured".
	Delivery *mailDelivery
	// CanHandOver is false while a required check is failing. Handing
	// over a broken deployment makes the customer's first experience an
	// error page, which is the one first impression worth blocking on.
	CanHandOver bool
	// HandoverBlockedBy names why, when it is false.
	HandoverBlockedBy string

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
	if !s.haveStore(w, r, lang) {
		return
	}

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
		if !s.acceptPost(w, r, lang) {
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
	ctx := r.Context()
	principal, err := s.Sessions.Principal(ctx)
	if err != nil {
		s.renderSetupNeeded(w, r, lang, http.StatusForbidden)
		return panel.Access{}, false
	}
	// The developer's own session, and the operator's, open this
	// directly. Both are here because they run the machine.
	if principal.Superadmin {
		return panel.Access{Principal: principal}, true
	}
	// An owner may too, once - see internal/panel/web/technicaldoor.go
	// for why this is a confirmation rather than a hidden page or an
	// open link. The confirmation is only "have they been warned"; the
	// authority is still the ownership check, asked here every request.
	if s.Sessions.TechnicalDoorOpen(ctx) && s.ownsAnySite(ctx, principal) {
		return panel.Access{Principal: principal}, true
	}
	// An owner who has not confirmed is sent to the door rather than
	// refused, because they may go through it. Anybody else gets the
	// page that says what this deployment is waiting for.
	if s.ownsAnySite(ctx, principal) {
		http.Redirect(w, r, TechnicalDoorPath, http.StatusSeeOther)
		return panel.Access{}, false
	}
	s.renderSetupNeeded(w, r, lang, http.StatusForbidden)
	return panel.Access{}, false
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
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
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

	// A refused submission is a client error, and says so. The wizard
	// used to answer 200 for these, which reads fine in a browser and
	// lies to everything else - a test, a script, an access log
	// somebody is scanning for the moment an installation went wrong.
	// The rest of the panel already answers 400 here; this is the same
	// rule rather than a second one.
	status := http.StatusOK
	if data.Failed {
		status = http.StatusBadRequest
	}

	s.Renderer.Render(w, r, status, "kurulum_"+step.ID, &ui.Page{
		L:       lang,
		Title:   lang.T("kurulum.adim." + step.ID + ".baslik"),
		Heading: lang.T("kurulum.adim." + step.ID + ".baslik"),
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
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
		results := s.Preflight.Run(ctx, s.preflightConfig())
		data.Checks = databaseChecks(results)
		data.Complete, data.Blocking = preflight.Complete(data.Checks)
		data.Ran = true

	case "siteler":
		value, err := s.Store.GetSetting(ctx, panel.KeyBeaconSites, "")
		if err != nil {
			return err
		}
		data.Sites = toStringList(value)
		data.SitesRaw = strings.Join(data.Sites, "\n")

	case "gorunum":
		rows, err := s.visibilityRows(ctx, lang)
		if err != nil {
			return err
		}
		data.Visible = rows

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
		// This step used to refuse itself when no site was configured,
		// because analytics retention was per site and there was nothing
		// to set it on. That setting has moved to the services' config
		// files; what is left is deployment-wide and needs no site, so
		// the refusal would now block a step that works.

	case "kontrol":
		data.Manual = preflight.ManualSteps()
		data.Unchecked = preflight.UncheckedSteps()
		if data.Ran {
			data.Complete, data.Blocking = preflight.Complete(data.Checks)
		}

	case "devir":
		if err := s.loadHandover(ctx, lang, data); err != nil {
			return err
		}
	}
	return nil
}

// loadHandover fills in the last step: whether the deployment may be
// handed over, and the invitations already open.
//
// The checks are run here rather than trusted from the previous step.
// An installer can reach this page directly, and "the checks passed
// when you looked at them" is not the same claim as "the checks pass".
func (s *Server) loadHandover(ctx context.Context, lang *ui.Language, data *setupPage) error {
	results := s.Preflight.Run(ctx, s.preflightConfig())
	ok, blocking := preflight.Complete(results)
	data.CanHandOver = ok
	if !ok {
		names := make([]string, 0, len(blocking))
		for _, check := range blocking {
			names = append(names, check.Label)
		}
		data.HandoverBlockedBy = strings.Join(names, ", ")
	}

	claims, err := s.Store.OpenOwnerClaims(ctx)
	if err != nil {
		return err
	}
	data.Claims = claims

	// An account already exists, so handover has happened. Said plainly
	// rather than by hiding the form: a developer coming back to add a
	// second owner should find out here, not by being refused.
	users, err := s.Store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if users > 0 && data.Message == "" {
		data.Message = lang.Tn("kurulum.devir.hesap_var", users, strconv.Itoa(users))
	}
	return nil
}

// retentionKeys are the settings the retention step touches. It carries
// legal weight - access logs contain IP addresses - so it is behind the
// developer password, which is why this step is in the developer's
// wizard and not the owner's.
//
// # Analytics retention used to be here and is not any more
//
// It moved to the services' config files. Log retention stays because
// the two are not the same kind of decision: logs are the deployment's
// own operational record, written by processes the installer runs, while
// the analytics tables are the visitors' data. Both have legal weight,
// but only one of them is somebody else's.
//
// The step keeps its per-site plumbing below, unused for now. It is
// three small types and it is what any future per-site value on this
// page will need; deleting it to add it back is not an improvement.
var retentionKeys = []panel.Key{
	panel.KeyLogRetentionDays,
}

// retentionRow is one editable value on the retention step.
//
// Site scoping is respected rather than averaged over: log retention is
// one number for the deployment, and a site-scoped value belongs to a
// site. Writing a site-scoped key globally is not a simplification, it
// is a value the store refuses - which is how this was found.
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

// retentionRows builds the page: the deployment-wide log retention.
//
// One row today. It reads from retentionKeys rather than naming the key
// so that the step does not have to be rewritten to gain a second one.
func (s *Server) retentionRows(ctx context.Context, access panel.Access) ([]retentionRow, error) {
	global, err := s.Store.SettingsView(ctx, access, "")
	if err != nil {
		return nil, err
	}
	rows := []retentionRow{}
	for _, view := range global {
		if slices.Contains(retentionKeys, view.Definition.Key) {
			rows = append(rows, retentionRow{
				Field: retentionFieldName(view.Definition.Key, ""),
				View:  view,
			})
		}
	}
	return rows, nil
}

// databaseChecks narrows the preflight to the ones this step is about.
//
// The full list belongs at the end, where it is the handover. Showing
// all of it here as well would train the installer to scroll past it
// the second time.
func databaseChecks(all []preflight.CheckResult) []preflight.CheckResult {
	out := make([]preflight.CheckResult, 0, len(all))
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
	data := setupPage{}

	switch step.ID {
	case "siteler":
		s.saveSites(r, lang, access, &data)
	case "gorunum":
		s.saveVisible(r, lang, access, &data)
	case "saklama":
		s.saveRetention(r, lang, access, &data)
	case "kontrol":
		results := s.Preflight.Run(r.Context(), s.preflightConfig())
		data.Checks = results
		data.Ran = true
	case "devir":
		s.handOver(r, lang, access, &data)
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
		// settingProblem, not err.Error(). The registry's validation
		// messages are written for the installer and naming the refused
		// value is the point - but the same call also returns wrapped
		// database errors, and those carry constraint names and query
		// text into a browser. See settingProblem.
		data.Message, data.Failed = s.settingProblem(lang, err), true
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
		data.Message, data.Failed = s.settingProblem(lang, err), true
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
		if err := s.Store.ApplySetting(r.Context(), access, c.key, c.site, c.value, auth,
			// No operation record: the wizard is a run of many settings
			// at once, and one operation per field would be noise
			// standing in for the single thing that happened.
			nil); err != nil {
			data.Message, data.Failed = s.settingProblem(lang, err), true
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
	if !s.haveStore(w, r, lang) {
		return
	}
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

func (s *Server) preflightConfig() preflight.Config {
	cfg := s.PreflightConfig
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

// handOver mints the invitation that turns this installation into
// somebody's panel.
//
// The two conditions it enforces are the reason the step exists rather
// than a link in the documentation:
//
//   - **Required checks must pass.** Handing over a deployment whose
//     schema is unapplied or whose roles are wrong makes the customer's
//     first experience an error page, and the person who could have
//     fixed it has just walked away.
//   - **The address must not already have an account.** Otherwise the
//     link is minted, handed over, and refused at the far end - by which
//     time the developer is gone.
func (s *Server) handOver(r *http.Request, lang *ui.Language, access panel.Access, data *setupPage) {
	ctx := r.Context()

	// Re-run rather than trust a hidden field. The form the installer is
	// submitting was drawn from a check that may be minutes old, and
	// nothing stops it being replayed.
	results := s.Preflight.Run(ctx, s.preflightConfig())
	if ok, blocking := preflight.Complete(results); !ok {
		// Names them, exactly as the page does when it draws the form
		// disabled. A refusal that only says "a check is failing" sends
		// the installer back to a list of fourteen to find out which.
		names := make([]string, 0, len(blocking))
		for _, check := range blocking {
			names = append(names, check.Label)
		}
		data.Message = lang.Tf("kurulum.devir.engelli_detay", strings.Join(names, ", "))
		data.Failed = true
		return
	}

	email := strings.TrimSpace(r.PostFormValue("eposta"))
	name := strings.TrimSpace(r.PostFormValue("ad"))
	if email == "" {
		data.Message, data.Failed = lang.T("kurulum.devir.eposta_bos"), true
		return
	}

	token, claim, err := s.Store.CreateOwnerClaim(ctx, email, name, access.Principal, 0)
	if err != nil {
		if errors.Is(err, panel.ErrEmailTaken) {
			data.Message, data.Failed = lang.T("kurulum.devir.eposta_kayitli"), true
			return
		}
		s.logger().Error("panel: creating owner invitation", "err", err)
		data.Message, data.Failed = lang.T("hesap.hata.kaydedilemedi"), true
		return
	}

	_ = s.Store.RecordFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionSetupCompleted,
		Detail: map[string]any{"invited": claim.Email, "claim_id": claim.ID},
	})

	data.ClaimURL = s.absoluteURL(r, ClaimPathPrefix+token)
	data.ClaimEmail = claim.Email
	data.Message = lang.T("kurulum.devir.olusturuldu")

	// Emailed as well, if this deployment can. The link above is set
	// first and unconditionally, which is the whole arrangement: mail is
	// a second copy of something the installer is already looking at,
	// and a send that fails changes what the page says beside the link
	// rather than whether there is one.
	delivery := s.deliverLink(ctx, lang, claim.Email,
		"posta.davet.konu", "posta.davet.govde", data.ClaimURL)
	data.Delivery = &delivery
}

// absoluteURL builds a link the installer can copy into a message.
//
// Assembled from the request rather than from configuration, because the
// panel does not know its own public address - it is behind whatever
// proxy the deployment put there. The Host header is the browser's own
// idea of where it is, which is exactly right for a link that same
// browser's owner is about to send somebody.
//
// It is never used for a redirect or a security decision, only printed:
// a forged Host here produces a link that does not work, not a link that
// goes somewhere else.
func (s *Server) absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}
