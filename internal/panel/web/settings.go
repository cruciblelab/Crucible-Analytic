package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// settingsPathSuffix is the settings page under a site.
//
// Under a site rather than at the top level, because Store.SettingsView
// resolves a site row over the deployment-wide one: the same key can be
// answered differently for two customers on one machine, and a page with
// no site in its URL would have to pick one silently.
const settingsPathSuffix = "/ayarlar"

// settingsPath is the link the navigation draws.
//
// PathEscape on the site id for the same reason memberPath does it: the
// id reaches the URL, and a site named with a slash would otherwise
// build a link to a different route.
func settingsPath(siteID string) string {
	if siteID == "" {
		return ""
	}
	return MembersPathPrefix + url.PathEscape(siteID) + settingsPathSuffix
}

// settingRow is one setting as the page draws it.
type settingRow struct {
	Key   string
	Label string
	Help  string
	// Value is the value in force, already turned into what the control
	// shows: a list joined with commas, a bool as a checkbox state.
	Value string
	// Checked is the bool control's state; Value carries "true"/"false"
	// for anything that needs the text.
	Checked bool

	// Kind decides which control the template draws.
	Kind string
	// Enum is the option list for a KindEnum setting.
	Enum []settingChoice
	// Min and Max bound a number control. Both zero means unbounded.
	Min, Max int

	// Editable is whether there is a control at all. False draws Lock
	// instead, and never draws a disabled control: a control somebody
	// can fill in and not submit is a worse answer than a sentence.
	Editable bool
	// Guarded means saving this one asks for the developer password.
	// Drawn so nobody meets the prompt as a surprise after typing a
	// value they will lose.
	Guarded bool
	// Lock is why there is no control. Never blank when Editable is
	// false - see panel.SettingView.
	Lock string
	// Reason is the legal explanation for a guarded setting.
	Reason string
	// Source is where the value came from: default, global or site.
	Source string
	// Developer marks a setting that only shows behind developer mode.
	Developer bool
	// Changed is true when something other than the default is stored,
	// which is what the reset control keys off.
	Changed bool
}

type settingChoice struct {
	Value    string
	Selected bool
}

// categoryLabelKey maps a category to its catalogue key.
//
// Written out rather than composed as "ayarlar.kategori." + category,
// which is what this was first. Two things break with the concatenated
// form and both are quiet: internal/panel/ui's dead-catalog check scans
// the source for literal keys and cannot see a composed one, so it
// reported all seven labels as unused text nobody draws; and a category
// whose label was never written would render its raw key on the page,
// at a customer, rather than failing here.
//
// TestEveryCategoryHasALabel checks both directions against
// panel.CategoryOrder.
var categoryLabelKey = map[panel.Category]string{
	panel.CatGorunum:  "ayarlar.kategori.gorunum",
	panel.CatToplama:  "ayarlar.kategori.toplama",
	panel.CatBot:      "ayarlar.kategori.bot",
	panel.CatGizlilik: "ayarlar.kategori.gizlilik",
	panel.CatSinirlar: "ayarlar.kategori.sinirlar",
	panel.CatTanilama: "ayarlar.kategori.tanilama",
	panel.CatBakim:    "ayarlar.kategori.bakim",
}

// settingSection is one category on the page.
type settingSection struct {
	Label string
	Rows  []settingRow
	// Open decides whether the section is expanded on arrival.
	//
	// Almost always false - a page whose sections are all open is the
	// flat list D4c replaced. It is true for the one section holding the
	// setting this render is about, and that exception is the whole
	// reason collapsing is safe: without it, a refused save draws a red
	// banner at the top of the page and hides the row that caused it, so
	// the customer is told something went wrong and shown nothing.
	Open bool
}

// settingsPage is Data for the settings template.
type settingsPage struct {
	SiteID   string
	Sections []settingSection
	// ShowDeveloper mirrors the viewer's own developer-mode preference.
	// Grouping, not permission: the server refuses a write on the
	// principal's access, never on this.
	ShowDeveloper bool
	// GateConfigured is false when no developer password is set, in
	// which case the guarded settings cannot be changed by anybody and
	// the page says so rather than offering a prompt that cannot pass.
	GateConfigured bool
	// Focus is the key this render is about: the one whose save was
	// refused, or whose developer-password prompt is open.
	//
	// It was AskingFor, written in three places and read in none - a
	// field that recorded a fact nothing used. Sections gave it a job:
	// it decides which one arrives expanded.
	Focus string

	// About is the credits and licence block. Built even for a viewer:
	// see about.go on why it is not gated.
	About aboutSection

	Message string
	Failed  bool
}

// settingsHandler serves and processes a site's settings.
//
// # Who reaches it
//
// Anybody who can see the site, including a viewer. That is deliberate
// and it is the opposite of the members page, which refuses outright.
//
// A viewer here gets every row with its value and a sentence saying why
// there is no control - which is what Store.SettingsView was built to
// return, LockNoticeViewer and all. Hiding the page instead would mean a
// customer cannot find out what their own deployment is set to without
// asking somebody, and PLAN.md D5 is explicit that a view is never
// hidden.
//
// What actually refuses a write is Store.ApplySetting, on the
// principal's access to that specific setting. The page's controls are
// cosmetic in exactly the way the navigation is: they exist so nobody is
// shown a door that will not open, not because they are the lock.
func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	siteID := r.PathValue("site")
	if siteID == "" {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}
	access, ok := s.siteAccess(w, r, p, siteID)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderSettings(w, r, lang, access, settingsPage{})
	case http.MethodPost:
		if !s.acceptPost(w, r, lang) {
			return
		}
		s.saveSetting(w, r, lang, access)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

// saveSetting applies one change.
//
// One setting per submit rather than the whole page, and that is a
// decision rather than a simplification. A form that saved twenty
// settings at once would have to report twenty outcomes, and the one
// that failed would be the one nobody read. It also keeps the guarded
// ones honest: the developer password authorizes the action it was
// typed for and nothing else, so a bulk save would either ask once for
// several guarded changes - spending one authorization on work the
// person may not have meant - or ask several times in one submit.
func (s *Server) saveSetting(w http.ResponseWriter, r *http.Request, lang *ui.Language, access panel.Access) {
	ctx := r.Context()
	key := panel.Key(r.PostFormValue("anahtar"))

	def, ok := panel.DefinitionFor(key)
	if !ok {
		// An unknown key is not a validation failure to explain, it is a
		// request that did not come from this page.
		s.renderSettings(w, r, lang, access, settingsPage{
			Message: lang.T("ayarlar.hata.bilinmeyen"), Failed: true})
		return
	}

	// The site a global setting is written against is "", not the site
	// in the URL. Taken from the definition rather than from the form:
	// a request that could choose its own scope could write one
	// customer's value into the deployment-wide row.
	site := ""
	if def.Scope == panel.ScopeSite {
		site = access.SiteID
	}

	// The authorization, if this setting needs one. The action name
	// comes from the definition, never from the request.
	//
	// That is the second layer and not the first, which is worth being
	// exact about: Store.SetGuardedSetting checks the authorization
	// against the key it is about to write, so an authorization minted
	// for one setting is refused there even if this handler were wrong
	// about which action to ask for. See
	// panel.TestGuardedSettings_AnAuthorizationForOneSettingDoesNotWriteAnother.
	//
	// An earlier version of this comment claimed the derivation here was
	// what prevented it. A test written against that claim failed on
	// correct code, which is how the overstatement was found.
	var auth devgate.Authorization
	if def.RequiresDeveloperPassword {
		var page settingsPage
		auth, page, ok = s.authorizeSetting(ctx, r, lang, access, key)
		if !ok {
			s.renderSettings(w, r, lang, access, page)
			return
		}
	}

	// The operation record opens before the work, because its id has to
	// exist in time to be attached to the log lines the work produces.
	// See internal/panel/operations.go and internal/logsink.
	//
	// A failure to open one does not stop the change. The operation log
	// is diagnostic; refusing a customer's legitimate setting change
	// because the diagnostics are unavailable would be the tail wagging
	// the dog.
	op, opErr := s.Store.BeginOperation(ctx, access, panel.ActionSettingChanged, string(key), site)
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record", "err", opErr, "key", key)
	}
	log := s.logger().With(logsink.OperationKey, op.ID(), logsink.SiteKey, site)

	before, _ := s.Store.GetSetting(ctx, key, site)

	var err error
	var msg string
	var value any
	switch r.PostFormValue("islem") {
	case "sifirla":
		op.Step("varsayilana dondur", true, "")
		err = s.Store.ClearSetting(ctx, access, key, site, auth, op)
		msg = lang.T("ayarlar.sifirlandi")
	case "kaydet":
		value, err = parseSettingValue(def, r)
		op.Step("degeri ayristir", err == nil, "")
		if err == nil {
			err = s.Store.ApplySetting(ctx, access, key, site, value, auth, op)
			msg = lang.T("ayarlar.kaydedildi")
		}
	default:
		err = errors.New("unknown operation")
	}

	if err != nil {
		op.Step("uygula", false, "")
		log.Warn("panel: settings change refused", "err", err, "key", key, "site", site)
		// rolled_back is false rather than nil: ApplySetting writes and
		// audits in one path and refuses before either, so a failure
		// here left nothing behind - but "nothing was applied" and
		// "nothing needed undoing" are the same fact only because that
		// ordering holds, and saying false records the fact rather than
		// the assumption.
		notRolledBack := false
		_ = op.Finish(ctx, outcomeFor(err), err, &notRolledBack)

		// Focus, and it is load-bearing now rather than informational.
		// The sections arrive collapsed, so a refusal that did not name
		// its key would render as a red banner over seven closed
		// headings - the customer told that something failed and shown
		// nothing that could have.
		s.renderSettings(w, r, lang, access, settingsPage{
			Message: settingErrorText(lang, def, err), Failed: true,
			Focus: string(key)})
		return
	}

	op.Step("uygula", true, "")
	after, _ := s.Store.GetSetting(ctx, key, site)
	op.Values(ctx, before, after)
	log.Info("panel: setting changed", "key", key, "site", site)
	_ = op.Finish(ctx, panel.OutcomeSucceeded, nil, nil)

	s.renderSettings(w, r, lang, access, settingsPage{Message: msg})
}

// outcomeFor tells a refusal from a fault.
//
// Both end the operation without the change being made, and they need
// different reactions: a refusal is the system working as designed - a
// missing capability, a wrong password, a value out of bounds - and
// burying those among genuine faults is how a real one gets missed in a
// list of them.
func outcomeFor(err error) panel.Outcome {
	switch {
	case errors.Is(err, panel.ErrSettingNotWritable),
		errors.Is(err, panel.ErrDeveloperPasswordRequired),
		errors.Is(err, panel.ErrUnknownSetting),
		errors.Is(err, panel.ErrPreconditionUnmet),
		errors.Is(err, panel.ErrInvalidSetting):
		return panel.OutcomeRefused
	default:
		return panel.OutcomeFailed
	}
}

// authorizeSetting verifies the developer password for one setting.
//
// Returns a page rather than writing a response, so the caller renders
// once on every path - a handler with two render sites grows a third.
func (s *Server) authorizeSetting(ctx context.Context, r *http.Request, lang *ui.Language,
	access panel.Access, key panel.Key) (devgate.Authorization, settingsPage, bool) {

	if s.Gate == nil || !s.Gate.Configured() {
		// No password configured means nobody can change these, and
		// that is the safe direction rather than an oversight. Said
		// plainly instead of showing a prompt that cannot succeed.
		return devgate.Authorization{},
			settingsPage{Message: lang.T("ayarlar.hata.kapi_yok"), Failed: true, Focus: string(key)}, false
	}

	password := r.PostFormValue("gelistirici_parolasi")
	if password == "" {
		// Failed, not just a prompt. The first version of this left it
		// unset, so a request to change a guarded setting with no
		// password rendered the "this needs a password" sentence with a
		// 200 - the setting was correctly untouched and the status said
		// otherwise.
		//
		// A test caught it, and the reason it is worth the words: the
		// status is what a script, a screen reader and an access log
		// all read first, and none of them reads the sentence.
		return devgate.Authorization{}, settingsPage{
			Message: lang.T("ayarlar.parola_gerekli"),
			Failed:  true,
			Focus:   string(key),
		}, false
	}

	action := panel.GateAction(key)
	result := s.Gate.Verify(ctx, devgate.Request{
		// One action, named from the definition. A list built from the
		// request would be a request that authorizes itself.
		Actions:   []string{action},
		Password:  password,
		Actor:     access.Principal.Label,
		ActorKind: string(access.Principal.Kind),
		ActorID:   access.Principal.UserID,
		// r.RemoteAddr, never a forwarded header: the record answers
		// "where was this password typed from", and a value the person
		// typing it can set is not an answer.
		Peer: peerAddr(r).String(),
	})
	if !result.OK() {
		return devgate.Authorization{}, settingsPage{
			Message: gateRefusalText(lang, result),
			Failed:  true,
			Focus:   string(key),
		}, false
	}
	return result.For(action), settingsPage{}, true
}

// parseSettingValue turns what the form sent into what the definition
// expects.
//
// Everything is re-parsed here and validated again in the store. That is
// not duplication: this turns text into a type and the store decides
// whether the value is admissible, and the store's answer is the one
// that counts because it is the one a non-browser caller also meets.
func parseSettingValue(def panel.Definition, r *http.Request) (any, error) {
	raw := strings.TrimSpace(r.PostFormValue("deger"))

	switch def.Kind {
	case panel.KindBool:
		// An unchecked checkbox sends nothing at all, which is the one
		// input shape where absence is a value rather than a mistake.
		return r.PostFormValue("deger") != "", nil

	case panel.KindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, errors.New("not a number")
		}
		return n, nil

	case panel.KindStringList:
		// Empty means the empty list, not a list with one empty entry.
		if raw == "" {
			return []string{}, nil
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out, nil

	default:
		return raw, nil
	}
}

// renderSettings draws the page, whatever happened before it.
//
// Reads the settings fresh on every render, including after a failed
// save. A page that redrew the value the person typed would show a
// deployment a value it is not running - and the moment somebody takes a
// screenshot of that, the screenshot is wrong about their own system.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access, data settingsPage) {

	views, err := s.Store.SettingsView(r.Context(), access, access.SiteID)
	if err != nil {
		s.logger().Error("panel: building the settings view", "err", err, "site", access.SiteID)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	data.SiteID = access.SiteID
	data.ShowDeveloper = access.ShowsTechnical()
	data.GateConfigured = s.Gate != nil && s.Gate.Configured()

	// Grouped, in CategoryOrder. A map would be one line shorter and
	// would reorder the page between reloads, because Go randomises map
	// iteration on purpose.
	byCategory := map[panel.Category][]settingRow{}
	for _, v := range views {
		// Developer settings are grouped away, not withheld. The server
		// refuses a write on access, never on this flag - see the
		// handler's comment and Definition.Developer.
		if v.Definition.Developer && !data.ShowDeveloper {
			continue
		}
		byCategory[v.Definition.Category] = append(
			byCategory[v.Definition.Category], settingRowFor(v))
	}
	for _, cat := range panel.CategoryOrder {
		rows := byCategory[cat]
		if len(rows) == 0 {
			// Not drawn. A heading that opens onto nothing reads as a
			// broken panel, and this happens for a real reason rather
			// than a mistake: a category whose settings are all
			// developer-only is empty for a customer with developer
			// mode off.
			continue
		}
		section := settingSection{
			Label: lang.T(categoryLabelKey[cat]),
			Rows:  rows,
		}
		for _, row := range rows {
			if data.Focus != "" && row.Key == data.Focus {
				section.Open = true
				break
			}
		}
		data.Sections = append(data.Sections, section)
	}
	data.About = s.aboutFor(lang)
	page := s.page(r, lang, access, "ayarlar", lang.T("ayarlar.baslik"))
	page.Site = ui.SiteView{ID: access.SiteID, Name: access.SiteID}
	page.Data = data
	if data.Message != "" {
		level := ui.NoticeInfo
		if data.Failed {
			level = ui.NoticeError
		}
		page.Notices = append(page.Notices, ui.Notice{Level: level, Body: data.Message})
	}
	// A refused save is not a 200 with a red banner. The status is what
	// a script, a screen reader and a log all read first, and a page
	// that says OK about a change it did not make is lying to every one
	// of them.
	status := http.StatusOK
	if data.Failed {
		status = http.StatusBadRequest
	}
	s.Renderer.Render(w, r, status, "ayarlar", page)
}

// settingRowFor turns one resolved setting into what the template draws.
func settingRowFor(v panel.SettingView) settingRow {
	def := v.Definition
	row := settingRow{
		Key:       string(def.Key),
		Label:     def.Label,
		Help:      def.Help,
		Kind:      string(def.Kind),
		Min:       def.Min,
		Max:       def.Max,
		Editable:  v.Access.Editable(),
		Guarded:   def.RequiresDeveloperPassword,
		Lock:      v.Lock,
		Reason:    v.Reason,
		Source:    v.Source,
		Developer: def.Developer,
		// "default" means nothing is stored, so there is nothing to
		// reset. Offering the control anyway would be a button that
		// does nothing, which is how a panel loses the benefit of the
		// doubt.
		Changed: v.Source != "default",
	}

	switch value := v.Value.(type) {
	case bool:
		row.Checked = value
		row.Value = strconv.FormatBool(value)
	case int:
		row.Value = strconv.Itoa(value)
	case []string:
		row.Value = strings.Join(value, ", ")
	case string:
		row.Value = value
	default:
		// A Kind this build renders no control for still shows its
		// value. Silence would read as "not set", which is a different
		// fact and the one that gets acted on.
		row.Value = fmt.Sprint(value)
		row.Editable = false
	}

	for _, option := range def.Enum {
		row.Enum = append(row.Enum, settingChoice{Value: option, Selected: option == row.Value})
	}
	return row
}

// settingErrorText turns a store error into a sentence a customer reads.
//
// Built from the definition rather than from the error, and the first
// version did the opposite. The validator says
//
//	logs.retention_days must be between 1 and 3650, got 999999
//
// which is correct, useful to a Go caller, and two problems on a page:
// it is English in a Turkish panel, and it is an internal message
// forwarded to a browser. The bound it names is already in the
// definition this handler is holding, so nothing is lost by saying it
// in the panel's own words.
//
// The raw error still reaches the log, which is where an operator wants
// the exact wording and where no customer is reading.
func settingErrorText(lang *ui.Language, def panel.Definition, err error) string {
	switch {
	case errors.Is(err, panel.ErrDeveloperPasswordRequired):
		return lang.T("ayarlar.hata.parola")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("ayarlar.hata.yetki")
	case errors.Is(err, panel.ErrUnknownSetting):
		return lang.T("ayarlar.hata.bilinmeyen")
	case errors.Is(err, panel.ErrPreconditionUnmet):
		// Its own branch, and it was missing. Without it this fell to
		// the bounds message below, so refusing privacy.ip_storage=full
		// on a deployment with no ip_hash_key answered "the value has to
		// be one of: full, masked" - about a value that is one of them.
		//
		// A message that sends the reader to check the thing that is
		// already correct is worse than a vague one: they will look,
		// find nothing wrong, and stop believing the page.
		return lang.T("ayarlar.hata.onkosul")
	}

	// The bound, when there is one. "Geçersiz değer" on its own is the
	// message that ends in a support channel.
	switch {
	case def.Kind == panel.KindInt && (def.Min != 0 || def.Max != 0):
		return lang.Tf("ayarlar.hata.aralik", def.Min, def.Max)
	case def.Kind == panel.KindEnum && len(def.Enum) > 0:
		return lang.Tf("ayarlar.hata.secenek", strings.Join(def.Enum, ", "))
	default:
		return lang.T("ayarlar.hata.gecersiz")
	}
}

// gateRefusalText says why the password was not accepted.
//
// Throttled and wrong are told apart, because they need different things
// from the reader: one is "check what you typed", the other is "wait".
// Neither says how many attempts remain - a counter is a measurement of
// the operator's failure budget, and it is more useful to somebody
// guessing than to somebody who mistyped.
func gateRefusalText(lang *ui.Language, result devgate.Result) string {
	switch result.Decision {
	case devgate.DecisionThrottled, devgate.DecisionBusy:
		return lang.T("ayarlar.hata.bekle")
	default:
		return lang.T("ayarlar.hata.parola_yanlis")
	}
}
