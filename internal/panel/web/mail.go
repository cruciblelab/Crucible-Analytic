package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/mail"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// The outgoing mail page.
//
// # What this page is actually for
//
// Not "can we send email". The panel already works without email -
// invitations are links (C7.1) and password reset runs on recovery codes
// and an operator link (C7.2), both built that way on purpose. This adds
// a convenience.
//
// What it is for is the sentence somebody reads when it does not work.
// SMTP fails in a handful of specific, fixable ways that all surface as
// "authentication failed", and the difference between telling somebody
// that and telling them "your provider turned off SMTP passwords and you
// need an app password" is the difference between a five-minute setup
// and an abandoned one. internal/mail works out which situation it is;
// this page says it in their language and shows what the server actually
// replied underneath.
//
// # Why the owner, and not an admin
//
// This is one account for the whole deployment rather than a per-site
// setting, so the per-site capabilities do not describe it - the same
// reason developer-access decisions are guarded by ownership rather than
// by a Capability.
//
// And a developer principal is refused before ownership is even
// considered, for a reason worth stating plainly: whoever controls the
// outgoing mail server receives every password-reset link the panel
// sends. A redeemed developer link carries Superadmin, so ownsAnySite
// answers yes for it; asking that question first would let somebody with
// a shell on the server point mail at a host they control and then reset
// any account they liked. The developer is refused by kind.
//
// # What the page never has
//
// The password. Not masked, not as dots, not as a placeholder that
// round-trips. panel.MailAccount has no password field, so there is
// nothing here for a template to render even by accident. The form's
// password box is empty every time and means "leave what is stored
// alone" when submitted empty.

// MailPath is the outgoing mail page.
const MailPath = "/posta"

// mailProbeTimeout bounds the connection test.
//
// Eight seconds: somebody is watching a button. Long enough for a real
// server on a slow link, short enough that a blocked port answers while
// they are still looking at the screen rather than after they have gone
// to make tea and decided the panel is broken.
const mailProbeTimeout = 8 * time.Second

// mailPage is Data for the template.
type mailPage struct {
	// KeyMissing means no secret_key is configured, so no password can
	// be stored. The page still renders and says what to do about it -
	// the alternative, hiding the page, leaves somebody looking for a
	// mail setting that the product does have.
	KeyMissing bool

	Account panel.MailAccount

	// Form is what the boxes are filled with. Separate from Account so a
	// rejected submission comes back with what was typed rather than
	// with what is stored - retyping a form because it refused one field
	// is how people stop trusting a wizard.
	Form mailForm

	// Probe is the result of a connection test, when one was just run.
	Probe *mailProbeView

	// SPF and DMARC are what the sender domain publishes. Reported, never
	// enforced - see internal/mail/dns.go for why a check that blocks on
	// DNS teaches people to click past checks that matter.
	DNS *mailDNSView

	Message string
	Failed  bool
}

// mailForm is the form's state.
type mailForm struct {
	Host       string
	Port       string
	Encryption string
	Username   string
	FromAddr   string
	FromName   string
	Enabled    bool
	// TestTo is the address the test message goes to. Not stored:
	// it belongs to the moment somebody is setting this up.
	TestTo string
}

// mailProbeView is one connection test as the page draws it.
type mailProbeView struct {
	OK bool
	// Headline is the diagnosis, already turned into a sentence.
	Headline string
	// Advice is what to do about it. Empty when there is nothing useful
	// to add beyond the headline.
	Advice string
	// ServerSaid is the server's own words, shown under a label that
	// says they are the server's. Empty when the failure was local.
	ServerSaid string
	// Reached, Encrypted and Authenticated are the three facts worth
	// drawing as a checklist: they turn "it failed" into "it got this
	// far".
	Reached       bool
	Encrypted     bool
	Authenticated bool
	// AuthOffered is what the server said it supports, verbatim. Shown
	// because it is the evidence behind a needs-OAuth diagnosis, and an
	// operator arguing with their provider's support desk needs it.
	AuthOffered []string
	// SuggestedPort is offered when the diagnosis is an encryption
	// mismatch. Offered rather than applied - see mail.SuggestedPort.
	SuggestedPort     int
	SuggestedEncLabel string
	// Sent reports that a test message was actually delivered, as
	// opposed to a connection that merely authenticated.
	Sent bool
}

// mailDNSView is what the sender domain publishes.
type mailDNSView struct {
	Domain string

	SPFFound  bool
	SPFRecord string
	SPFPolicy string

	DMARCFound  bool
	DMARCRecord string
	DMARCPolicy string

	// Unavailable means the lookup itself failed - this machine's
	// resolver, not the domain. Drawn differently, because "we could not
	// ask" and "we asked and there is nothing" send an operator to two
	// different places.
	Unavailable bool
}

// mailHandler serves and processes the outgoing mail page.
func (s *Server) mailHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if !s.haveStore(w, r, lang) {
		return
	}
	p, ok := s.requireMailOwner(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderMail(w, r, lang, p, mailPage{})
	case http.MethodPost:
		if !s.acceptPost(w, r, lang) {
			return
		}
		s.postMail(w, r, lang, p)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

// requireMailOwner resolves somebody entitled to configure outgoing mail.
//
// Kind before ownership, and this ordering is load-bearing rather than
// tidy. Whoever controls the outgoing mail server receives every
// password-reset link this panel sends, so "may configure mail" is very
// close to "may become any user". A redeemed developer link carries
// Superadmin and would satisfy ownsAnySite; checking the kind first is
// what stops somebody with a shell on the server from pointing mail at a
// host they control.
//
// 403 rather than 404: an admin knows the deployment sends mail, and
// "you are not the one who configures this" is more useful than
// pretending the page does not exist.
func (s *Server) requireMailOwner(w http.ResponseWriter, r *http.Request) (panel.Principal, bool) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return panel.Principal{}, false
	}
	lang := s.language(r)
	if p.Kind != panel.PrincipalUser {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return panel.Principal{}, false
	}
	if !s.ownsAnySite(r.Context(), p) {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return panel.Principal{}, false
	}
	return p, true
}

// postMail dispatches the page's three actions.
func (s *Server) postMail(w http.ResponseWriter, r *http.Request, lang *ui.Language, p panel.Principal) {
	switch r.PostFormValue("islem") {
	case "kaydet":
		s.saveMail(w, r, lang, p)
	case "dogrula":
		s.verifyMail(w, r, lang, p, "")
	case "deneme":
		s.verifyMail(w, r, lang, p, strings.TrimSpace(r.PostFormValue("deneme_adres")))
	case "sil":
		s.deleteMail(w, r, lang, p)
	default:
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
	}
}

func (s *Server) saveMail(w http.ResponseWriter, r *http.Request, lang *ui.Language, p panel.Principal) {
	form := mailFormFrom(r)
	page := mailPage{Form: form}

	key := s.SecretKey
	page.KeyMissing = !key.IsSet()

	port, err := strconv.Atoi(strings.TrimSpace(form.Port))
	if err != nil {
		page.Failed, page.Message = true, lang.T("posta.hata.port")
		s.renderMail(w, r, lang, p, page)
		return
	}

	in := panel.MailAccountInput{
		Host:          form.Host,
		Port:          port,
		Encryption:    mail.Encryption(form.Encryption),
		Username:      form.Username,
		Password:      r.PostFormValue("sifre"),
		ClearPassword: r.PostFormValue("sifre_sil") != "",
		FromAddress:   form.FromAddr,
		FromName:      form.FromName,
		Enabled:       form.Enabled,
	}

	if err := s.Store.SaveMailAccount(r.Context(), key, in, p.UserID); err != nil {
		page.Failed = true
		page.Message = mailSaveMessage(lang, err)
		s.renderMail(w, r, lang, p, page)
		return
	}

	_ = s.Store.RecordFor(r.Context(), p, panel.AuditEntry{
		Action: panel.ActionMailSaved,
		Target: in.Host,
		Detail: map[string]any{
			"port": in.Port, "encryption": string(in.Encryption), "enabled": in.Enabled,
		},
	})
	page.Message = lang.T("posta.kaydedildi")
	s.renderMail(w, r, lang, p, page)
}

// mailSaveMessage turns a store error into the sentence for it.
//
// The one that matters is a missing key: "a mail password cannot be
// saved" is not a bug report, it is a line to add to a config file, and
// the page has to say which line.
func mailSaveMessage(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, sealed.ErrNoKey):
		return lang.T("posta.hata.anahtar_yok")
	case errors.Is(err, mail.ErrInvalidAddress):
		return lang.T("posta.hata.gonderen")
	default:
		return lang.T("posta.hata.kaydedilemedi")
	}
}

// verifyMail runs a connection test, and optionally sends a message.
//
// One handler for both because they are the same conversation up to the
// last three commands, and splitting them would let the two drift into
// disagreeing about what a working account looks like.
func (s *Server) verifyMail(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, testTo string) {

	page := mailPage{Form: mailFormFrom(r)}
	page.Form.TestTo = testTo

	key := s.SecretKey
	page.KeyMissing = !key.IsSet()

	cfg, err := s.Store.MailConfig(r.Context(), key)
	if err != nil {
		page.Failed = true
		switch {
		case errors.Is(err, panel.ErrNoMailAccount):
			page.Message = lang.T("posta.hata.once_kaydet")
		case errors.Is(err, sealed.ErrCannotOpen):
			page.Message = lang.T("posta.hata.acilamiyor")
		default:
			page.Message = lang.T("posta.hata.okunamadi")
		}
		s.renderMail(w, r, lang, p, page)
		return
	}
	cfg.Timeout = mailProbeTimeout

	var probe mail.Probe
	if testTo != "" {
		probe = cfg.Send(mail.Message{
			To:      testTo,
			Subject: lang.T("posta.denemeileti.konu"),
			Body:    lang.T("posta.denemeileti.govde"),
		})
	} else {
		probe = cfg.Probe()
	}

	// Recorded whichever way it went. The value of storing it is the
	// question asked three weeks later - "why did nobody get the
	// invitation" - and a panel that only remembers its successes cannot
	// answer it.
	if err := s.Store.RecordMailVerification(r.Context(), probe); err != nil {
		s.logger().Error("panel: recording the mail verification", "err", err)
	}

	view := mailProbeViewFor(lang, cfg, probe)
	page.Probe = &view
	page.Failed = !probe.OK()

	// The domain's records, looked up here rather than on every render.
	//
	// Two DNS queries on the way to drawing a form is up to three
	// seconds of somebody waiting to type in a box, on a page they may
	// only be opening to turn sending off. It also belongs with the test
	// result rather than with the form: "here is what I found out about
	// your setup" is one answer, and splitting half of it above the
	// button makes it look like a precondition.
	page.DNS = s.mailDNS(r.Context(), cfg.From)

	_ = s.Store.RecordFor(r.Context(), p, panel.AuditEntry{
		Action: panel.ActionMailVerified,
		Target: cfg.Host,
		Detail: map[string]any{
			"diagnosis": string(probe.Diagnose()),
			"sent":      probe.Sent,
			"stage":     string(probe.Stage),
		},
	})
	s.renderMail(w, r, lang, p, page)
}

func (s *Server) deleteMail(w http.ResponseWriter, r *http.Request, lang *ui.Language, p panel.Principal) {
	if err := s.Store.DeleteMailAccount(r.Context()); err != nil {
		s.logger().Error("panel: deleting the mail account", "err", err)
		s.renderMail(w, r, lang, p, mailPage{Failed: true, Message: lang.T("posta.hata.silinemedi")})
		return
	}
	_ = s.Store.RecordFor(r.Context(), p, panel.AuditEntry{Action: panel.ActionMailDeleted})
	s.renderMail(w, r, lang, p, mailPage{Message: lang.T("posta.silindi")})
}

// mailFormFrom reads the submitted form.
func mailFormFrom(r *http.Request) mailForm {
	return mailForm{
		Host:       strings.TrimSpace(r.PostFormValue("sunucu")),
		Port:       strings.TrimSpace(r.PostFormValue("port")),
		Encryption: r.PostFormValue("sifreleme"),
		Username:   strings.TrimSpace(r.PostFormValue("kullanici")),
		FromAddr:   strings.TrimSpace(r.PostFormValue("gonderen")),
		FromName:   strings.TrimSpace(r.PostFormValue("gonderen_ad")),
		Enabled:    r.PostFormValue("acik") != "",
	}
}

// mailProbeViewFor turns a probe into what the page draws.
func mailProbeViewFor(lang *ui.Language, cfg mail.Config, p mail.Probe) mailProbeView {
	diagnosis := p.Diagnose()
	view := mailProbeView{
		OK:            p.OK() && p.Err == nil,
		Reached:       p.Reached,
		Encrypted:     p.Encrypted(),
		Authenticated: p.Authenticated,
		AuthOffered:   p.AuthOffered,
		ServerSaid:    p.ServerSaid,
		Sent:          p.Sent,
	}
	if view.ServerSaid != "" && p.ServerCode > 0 {
		view.ServerSaid = strconv.Itoa(p.ServerCode) + " " + view.ServerSaid
	}
	if view.ServerSaid == "" {
		view.ServerSaid = p.Detail
	}

	if diagnosis == mail.DiagOK {
		view.Headline = lang.T("posta.sonuc.tamam")
		if p.Sent {
			view.Headline = lang.T("posta.sonuc.gonderildi")
		}
		return view
	}

	// The diagnosis is a closed set and every member has a pair of
	// catalog keys. internal/panel/ui's tests hold that open from the
	// other side: a diagnosis with no sentence is a missing key, and a
	// key with no diagnosis is a dead entry.
	view.Headline = lang.T("posta.tani." + string(diagnosis))
	view.Advice = lang.T("posta.oneri." + string(diagnosis))

	if diagnosis == mail.DiagWrongPort {
		if port, enc, ok := cfg.SuggestedPort(); ok {
			view.SuggestedPort = port
			view.SuggestedEncLabel = lang.T("posta.mod." + string(enc))
		}
	}
	return view
}

// renderMail draws the page.
func (s *Server) renderMail(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, data mailPage) {

	ctx := r.Context()

	key := s.SecretKey
	data.KeyMissing = !key.IsSet()

	acc, err := s.Store.MailAccount(ctx, key)
	if err != nil {
		s.logger().Error("panel: reading the mail account", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	data.Account = acc

	// The form is filled from what is stored only when nothing was
	// submitted. A rejected submission keeps what was typed - the
	// alternative silently discards somebody's work and shows them the
	// old values, which reads as the panel ignoring them.
	if data.Form.Host == "" && data.Form.FromAddr == "" {
		data.Form = mailFormOf(acc)
	}

	page := s.page(r, lang, panel.Access{Principal: p}, "posta", lang.T("posta.baslik"))
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
	s.Renderer.Render(w, r, status, "posta", page)
}

// mailFormOf fills the form from the stored account.
//
// Deliberately has no password branch: there is no password on a
// MailAccount to copy, which is what keeps this function from being the
// place somebody adds one.
func mailFormOf(acc panel.MailAccount) mailForm {
	if !acc.Configured {
		return mailForm{
			Port:       strconv.Itoa(mail.DefaultPort(mail.EncryptionSTARTTLS)),
			Encryption: string(mail.EncryptionSTARTTLS),
		}
	}
	return mailForm{
		Host:       acc.Host,
		Port:       strconv.Itoa(acc.Port),
		Encryption: string(acc.Encryption),
		Username:   acc.Username,
		FromAddr:   acc.FromAddress,
		FromName:   acc.FromName,
		Enabled:    acc.Enabled,
	}
}

// mailDNS looks up what the sender domain publishes.
//
// Bounded and never fatal. These are two DNS queries on a page render,
// and a resolver having a bad afternoon must not be the reason somebody
// cannot reach their mail settings - so the whole thing has its own
// short deadline and a failure draws as "could not ask".
func (s *Server) mailDNS(ctx context.Context, from string) *mailDNSView {
	domain := mail.DomainOf(from)
	if domain == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	view := &mailDNSView{Domain: domain}
	spf := mail.LookupSPF(ctx, domain)
	dmarc := mail.LookupDMARC(ctx, domain)

	if spf.Err != nil && dmarc.Err != nil {
		view.Unavailable = true
		return view
	}
	view.SPFFound, view.SPFRecord, view.SPFPolicy = spf.Found, spf.Record, spf.AllQualifier
	view.DMARCFound, view.DMARCRecord, view.DMARCPolicy = dmarc.Found, dmarc.Record, dmarc.Policy
	return view
}
