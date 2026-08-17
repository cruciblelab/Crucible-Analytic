package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The customer's wizard: the first thing the person who bought this
// actually sees.
//
// It is the counterpart of the technical one, and the two are shaped by
// opposite rules. The technical wizard verifies more than it configures,
// because most of what a deployment needs cannot be set from a browser.
// This one configures and never verifies, because **it must never
// require a technical step**. Those were done before the customer
// arrived. Where it needs a technical value it shows what the developer
// configured rather than an empty field, so the page is an account of
// the deployment rather than homework.
//
// The other rule is that it starts by creating the account. Until this
// exists there is no way to make the first one at all - see
// internal/panel/ownerclaim.go - so the first step is not a form the
// customer could skip.

// WelcomePathPrefix is the owner's wizard.
const WelcomePathPrefix = "/hosgeldiniz/"

// ClaimPathPrefix is where an invitation link is opened.
const ClaimPathPrefix = "/sahiplen/"

// welcomeStep is one page of the owner's wizard.
//
// A closed, ordered list for the same reason the technical wizard has
// one: the order is the product, and a step reachable only by typing its
// name is a step nobody runs.
type welcomeStep struct {
	ID string
	// Writes reports whether the step changes anything. The last two do
	// not - one shows a snippet to copy, the other points at a page -
	// and saying so on the page keeps "I filled this in and nothing
	// happened" from being a fair complaint.
	Writes bool
}

var welcomeSteps = []welcomeStep{
	{ID: "site", Writes: true},
	{ID: "saat", Writes: true},
	{ID: "olcum"},
	{ID: "ekip"},
}

func welcomeIndex(id string) int {
	for i, step := range welcomeSteps {
		if step.ID == id {
			return i
		}
	}
	return -1
}

// welcomePage is Data for every owner-wizard template.
type welcomePage struct {
	Step   string
	Number int
	Total  int
	Last   bool
	// ReadOnly marks a step that changes nothing.
	ReadOnly bool
	Steps    []stepView
	BackURL  string
	NextURL  string

	// Sites is one row per configured site, with whatever name it has.
	Sites []siteNameRow
	// Timezone is the zone in force, and Configured is what the config
	// file says - shown so the customer can see what they are
	// overriding rather than replacing a value they never knew about.
	Timezone           string
	ConfiguredTimezone string
	// Zones is a short list of plausible choices. Not exhaustive: the
	// field accepts any zone this machine knows, and a select of six
	// hundred names is worse than a text field with examples.
	Zones []string

	// Snippet is the tag to embed, per site. Empty BeaconURL means the
	// panel was not told where the beacon is, and the step says so
	// rather than printing a snippet that points nowhere.
	BeaconURL string
	Snippets  []snippetRow

	// MembersURL is where the last step sends them.
	MembersURL string

	Message string
	Failed  bool
}

type siteNameRow struct {
	SiteID string
	Name   string
}

type snippetRow struct {
	SiteID  string
	Name    string
	Snippet string
}

// claimHandler opens an invitation and turns it into an account.
func (s *Server) claimHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, ClaimPathPrefix), "/")

	claim, err := s.Store.LookupOwnerClaim(r.Context(), token)
	if err != nil {
		if errors.Is(err, panel.ErrClaimInvalid) {
			// One page for unknown, expired and already-used. Telling
			// them apart would confirm to anybody guessing that a guess
			// had once been real - and a person holding a genuine link
			// that has expired does the same thing in every case: ask
			// for another.
			s.Renderer.Render(w, r, http.StatusNotFound, "sahiplen", &ui.Page{
				L:       lang,
				Title:   lang.T("sahiplen.gecersiz.baslik"),
				Heading: lang.T("sahiplen.gecersiz.baslik"),
				F:       ui.NewFormatter(lang, s.zone(r.Context())),
				Data:    claimPage{Invalid: true},
			})
			return
		}
		s.logger().Error("panel: looking up invitation", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderClaim(w, r, lang, http.StatusOK, claimPage{Claim: claim, Token: token})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.submitClaim(w, r, lang, claim, token)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

// claimPage is Data for the claim template.
type claimPage struct {
	Claim panel.OwnerClaim
	Token string
	// Invalid renders the "this link cannot be used" page instead of the
	// form.
	Invalid bool
	Error   string
}

func (s *Server) submitClaim(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	claim panel.OwnerClaim, token string) {

	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	ctx := r.Context()
	password := r.PostFormValue("parola")
	repeat := r.PostFormValue("parola_tekrar")
	name := strings.TrimSpace(r.PostFormValue("ad"))

	refuse := func(msg string) {
		s.renderClaim(w, r, lang, http.StatusBadRequest,
			claimPage{Claim: claim, Token: token, Error: msg})
	}
	if password != repeat {
		refuse(lang.T("hesap.hata.parolalar_farkli"))
		return
	}
	if err := panel.ValidatePassword(password); err != nil {
		refuse(passwordProblem(lang, err))
		return
	}
	if utf8.RuneCountInString(name) > panel.MaxDisplayNameLength {
		refuse(lang.T("hesap.hata.ad_uzun"))
		return
	}
	hash, err := panel.HashPassword(password)
	if err != nil {
		s.logger().Error("panel: hashing password", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	// The sites the account becomes owner of. Read here rather than
	// inside the store so the store stays a data layer that is told what
	// to do - and so this list is exactly what the technical wizard
	// configured, from the same setting the beacon reads.
	sites, err := s.configuredSites(ctx)
	if err != nil {
		s.logger().Error("panel: reading configured sites", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	user, err := s.Store.RedeemOwnerClaim(ctx, token, hash, sites, peerAddr(r))
	if err != nil {
		if errors.Is(err, panel.ErrClaimInvalid) {
			// It was open when the page was drawn and is not now: it
			// expired while the form was open, or another tab took it.
			// The second is the race this refusal exists for.
			refuse(lang.T("sahiplen.hata.artik_gecersiz"))
			return
		}
		s.logger().Error("panel: redeeming invitation", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	id := user.ID
	_ = s.Store.Record(ctx, panel.AuditEntry{
		Action: panel.ActionUserCreated, ActorKind: panel.PrincipalUser,
		ActorID: &id, ActorLabel: user.Email,
		Detail: map[string]any{"via": "owner_claim", "sites": len(sites)},
	})

	// Signed in immediately. The alternative - redirect to the sign-in
	// form - asks somebody to type a password they set four seconds ago,
	// which reads as the panel not having believed them.
	if err := s.Sessions.LogIn(ctx, user); err != nil {
		s.logger().Error("panel: signing in new owner", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	if err := s.Store.TouchLastLogin(ctx, user.ID); err != nil {
		s.logger().Warn("panel: touching last login", "err", err)
	}
	http.Redirect(w, r, WelcomePathPrefix+welcomeSteps[0].ID, http.StatusSeeOther)
}

func (s *Server) renderClaim(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	status int, data claimPage) {

	s.Renderer.Render(w, r, status, "sahiplen", &ui.Page{
		L:       lang,
		Title:   lang.T("sahiplen.baslik"),
		Heading: lang.T("sahiplen.baslik"),
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		Data:    data,
	})
}

// welcomeHandler serves the owner's wizard.
func (s *Server) welcomeHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// The wizard belongs to somebody who owns something. A developer
	// session has its own wizard, and a viewer has nothing to configure
	// here.
	if !s.ownsAnySite(r.Context(), p) {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return
	}

	id := strings.Trim(strings.TrimPrefix(r.URL.Path, WelcomePathPrefix), "/")
	if id == "" {
		http.Redirect(w, r, WelcomePathPrefix+welcomeSteps[0].ID, http.StatusSeeOther)
		return
	}
	index := welcomeIndex(id)
	if index < 0 {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderWelcome(w, r, lang, p, index, welcomePage{})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.saveWelcome(w, r, lang, p, index)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) saveWelcome(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, index int) {

	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	ctx := r.Context()
	var data welcomePage

	switch welcomeSteps[index].ID {
	case "site":
		data = s.saveSiteNames(ctx, lang, p, r)
	case "saat":
		data = s.saveTimezone(ctx, lang, p, strings.TrimSpace(r.PostFormValue("saat_dilimi")))
	default:
		// Nothing to save; move on rather than redisplaying a page the
		// reader has already finished with.
		http.Redirect(w, r, WelcomePathPrefix+welcomeSteps[min(index+1, len(welcomeSteps)-1)].ID,
			http.StatusSeeOther)
		return
	}
	s.renderWelcome(w, r, lang, p, index, data)
}

func (s *Server) saveSiteNames(ctx context.Context, lang *ui.Language, p panel.Principal,
	r *http.Request) welcomePage {

	sites, err := s.configuredSites(ctx)
	if err != nil {
		s.logger().Error("panel: reading configured sites", "err", err)
		return welcomePage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}

	// Only the sites this deployment is configured for, and only the
	// ones this principal owns. The form field names carry site ids from
	// the browser; iterating over the configured list rather than over
	// the submitted fields means a fabricated one names nothing.
	saved := 0
	for _, site := range sites {
		access, err := s.Store.AccessFor(ctx, p, site)
		if err != nil {
			s.logger().Error("panel: resolving access", "err", err, "site", site)
			return welcomePage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
		}
		if !access.Can(panel.CapManageSettings) {
			continue
		}
		name := strings.TrimSpace(r.PostFormValue("ad:" + site))
		if err := s.Store.SetSetting(ctx, panel.KeySiteName, site, name, actorOf(access)); err != nil {
			s.logger().Error("panel: saving site name", "err", err, "site", site)
			return welcomePage{Message: settingProblem(lang, err), Failed: true}
		}
		saved++
	}
	if saved == 0 {
		return welcomePage{Message: lang.T("hosgeldiniz.site.yok"), Failed: true}
	}
	return welcomePage{Message: lang.T("kaydedildi")}
}

func (s *Server) saveTimezone(ctx context.Context, lang *ui.Language, p panel.Principal,
	zone string) welcomePage {

	// A global setting, so it needs a global access decision rather than
	// a per-site one.
	access := panel.Access{Principal: p}
	if !s.ownsAnySite(ctx, p) {
		return welcomePage{Message: lang.T("hosgeldiniz.saat.yetki"), Failed: true}
	}
	if err := s.Store.SetSetting(ctx, panel.KeyPanelTimezone, "", zone, actorOf(access)); err != nil {
		return welcomePage{Message: settingProblem(lang, err), Failed: true}
	}
	return welcomePage{Message: lang.T("kaydedildi")}
}

func (s *Server) renderWelcome(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, index int, data welcomePage) {

	ctx := r.Context()
	step := welcomeSteps[index]
	data.Step = step.ID
	data.Number = index + 1
	data.Total = len(welcomeSteps)
	data.Last = index == len(welcomeSteps)-1
	data.ReadOnly = !step.Writes
	if index > 0 {
		data.BackURL = WelcomePathPrefix + welcomeSteps[index-1].ID
	}
	if index < len(welcomeSteps)-1 {
		data.NextURL = WelcomePathPrefix + welcomeSteps[index+1].ID
	}
	for i, other := range welcomeSteps {
		data.Steps = append(data.Steps, stepView{
			ID:      other.ID,
			Title:   lang.T("hosgeldiniz.adim." + other.ID + ".baslik"),
			URL:     WelcomePathPrefix + other.ID,
			Number:  i + 1,
			Current: i == index,
			Done:    i < index,
		})
	}

	sites, err := s.configuredSites(ctx)
	if err != nil {
		s.logger().Error("panel: reading configured sites", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	switch step.ID {
	case "site":
		for _, site := range sites {
			name, err := s.Store.GetSetting(ctx, panel.KeySiteName, site)
			if err != nil {
				s.logger().Error("panel: reading site name", "err", err, "site", site)
				s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
				return
			}
			text, _ := name.(string)
			data.Sites = append(data.Sites, siteNameRow{SiteID: site, Name: text})
		}

	case "saat":
		data.Timezone = s.storedTimezone(ctx)
		data.ConfiguredTimezone = s.ConfiguredTimezone
		data.Zones = suggestedZones

	case "olcum":
		data.BeaconURL = s.BeaconURL
		if data.BeaconURL != "" {
			for _, site := range sites {
				name, _ := s.Store.GetSetting(ctx, panel.KeySiteName, site)
				text, _ := name.(string)
				data.Snippets = append(data.Snippets, snippetRow{
					SiteID:  site,
					Name:    text,
					Snippet: beaconSnippet(data.BeaconURL, site),
				})
			}
		}

	case "ekip":
		if len(sites) > 0 {
			data.MembersURL = memberPath(sites[0])
		}
	}

	page := s.page(r, lang, panel.Access{Principal: p}, "hosgeldiniz",
		lang.T("hosgeldiniz.adim."+step.ID+".baslik"))
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
	s.Renderer.Render(w, r, status, "hosgeldiniz_"+step.ID, page)
}

// suggestedZones is a short list of examples, not a menu.
//
// The field takes any zone this machine knows. A select of six hundred
// IANA names is harder to use than a text box beside four plausible
// answers, and it would still be wrong for the fifth deployment.
var suggestedZones = []string{
	"Europe/Istanbul", "Europe/Berlin", "Europe/London", "UTC",
}

// beaconSnippet builds the tag to paste into a website.
//
// Assembled here rather than imported from internal/beacon, which the
// panel deliberately does not depend on - the panel and the beacon are
// separate processes with separate database roles, and a shared package
// between them is a shared upgrade. The shape is asserted by a test
// against the beacon's own documented form.
func beaconSnippet(baseURL, siteID string) string {
	return `<script defer src="` + strings.TrimRight(baseURL, "/") +
		`/_ca/ca.js" data-site="` + siteID + `"></script>`
}

// configuredSites reads the beacon's allowlist: the sites this
// deployment exists to measure.
func (s *Server) configuredSites(ctx context.Context) ([]string, error) {
	value, err := s.Store.GetSetting(ctx, panel.KeyBeaconSites, "")
	if err != nil {
		return nil, err
	}
	return toStringList(value), nil
}

// ownsAnySite reports whether this principal owns at least one site.
//
// A superadmin does, by running the deployment. Anybody else needs a
// membership row saying so - and an admin does not qualify: the owner's
// wizard sets what a site is called and which clock it is read against,
// which are the customer's decisions rather than their staff's.
func (s *Server) ownsAnySite(ctx context.Context, p panel.Principal) bool {
	if p.Superadmin {
		return true
	}
	if p.Kind != panel.PrincipalUser {
		return false
	}
	sites, err := s.Store.Sites(ctx, p, nil)
	if err != nil {
		s.logger().Error("panel: listing sites", "err", err)
		return false
	}
	for _, site := range sites {
		if site.Role == panel.RoleOwner {
			return true
		}
	}
	return false
}

// zone is the time zone this request renders in.
//
// The stored setting wins over the config file, because the customer
// knows their own timezone better than whoever installed the deployment.
// An unreadable or unset setting falls back rather than failing: a page
// that will not render because a zone name is wrong is worse than a page
// that renders in the zone it always used.
func (s *Server) zone(ctx context.Context) *time.Location {
	if name := s.storedTimezone(ctx); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
		// Stored and unloadable. checkTimezone refuses this on the way
		// in, so reaching here means the machine's zone database
		// changed under a value that was valid when it was written -
		// worth a log line and not worth a broken page.
		s.logger().Warn("panel: stored timezone cannot be loaded", "zone", name)
	}
	if s.Zone != nil {
		return s.Zone
	}
	return time.UTC
}

func (s *Server) storedTimezone(ctx context.Context) string {
	if s.Store == nil {
		return ""
	}
	value, err := s.Store.GetSetting(ctx, panel.KeyPanelTimezone, "")
	if err != nil {
		s.logger().Error("panel: reading timezone setting", "err", err)
		return ""
	}
	name, _ := value.(string)
	return name
}

// settingProblem turns a settings error into a sentence.
//
// Validate's messages already name the setting and say what is wrong, in
// Turkish, beside the rule that produced them - so they are shown rather
// than mapped. The mapping exists only for the ones that are not about
// the value at all.
func settingProblem(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, panel.ErrDeveloperPasswordRequired):
		return lang.T("hosgeldiniz.hata.gelistirici_parolasi")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("hosgeldiniz.hata.yetki")
	case err == nil:
		return ""
	default:
		return err.Error()
	}
}
