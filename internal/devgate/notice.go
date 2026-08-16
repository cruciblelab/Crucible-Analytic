package devgate

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// The Turkish text the panel shows. It lives here rather than in a
// template so that every place that puts up the prompt says the same
// thing, and so that changing what the rule *is* and changing what the
// rule *says* are one edit rather than two.
//
// The wording is deliberate on one point: it names the developer as the
// author of the rule. The person hitting this prompt is usually the
// customer, in their own panel, on their own server, and being stopped
// by a password they do not have. Told nothing, they conclude the panel
// is broken. Told that a rule exists and why, they can decide whether to
// call us - which is the correct outcome, because the settings behind
// this prompt are exactly the ones somebody should be called about.
const (
	// Notice explains the rule itself.
	Notice = "Bu ayar, geliştirici şifresi ister. Geliştirici, hukuki sorun " +
		"çıkarabilecek alanlar için şifre kuralı getirdi: buradaki ayarlar " +
		"hangi kişisel verinin saklandığını ve ne kadar süre tutulduğunu " +
		"değiştirir. Şifre her değişiklikte yeniden sorulur; oturum açık kalmaz."

	// NoticeNotConfigured is what to say when the deployment has no
	// developer password at all. The setting is not broken and the
	// person is not doing anything wrong - the change simply cannot be
	// made from here, and the honest thing is to say who can make it.
	NoticeNotConfigured = "Bu kurulumda geliştirici şifresi tanımlı değil, bu yüzden " +
		"korumalı ayarlar değiştirilemez ve varsayılan (en korumacı) " +
		"değerlerinde kalır. Tanımlamak sunucuya erişim gerektirir: " +
		"yapılandırma dosyasındaki [developer] bölümüne password_hash eklenir."
)

// Explain turns a decision into the sentence to show the person who just
// tried.
//
// Nothing here is hidden to avoid an oracle, and that is a considered
// choice rather than an oversight: whoever sees this message has already
// authenticated to the panel and has already been let into developer
// mode. Telling them "wrong password" instead of something vague costs
// nothing an attacker in that position does not already have, and being
// vague would send an operator hunting for a fault that is not there.
func Explain(r Result) string {
	switch r.Decision {
	case DecisionGranted:
		return ""
	case DecisionNoPassword:
		return "Geliştirici şifresi girilmedi."
	case DecisionWrongPassword:
		return "Geliştirici şifresi yanlış."
	case DecisionNotConfigured:
		return NoticeNotConfigured
	case DecisionThrottled:
		return fmt.Sprintf("Çok fazla hatalı deneme yapıldı. Yaklaşık %s sonra tekrar deneyin.",
			humanDuration(r.RetryAfter))
	case DecisionBusy:
		return "Şu anda başka bir doğrulama sürüyor. Birkaç saniye sonra tekrar deneyin."
	case DecisionNoAction:
		return "Doğrulanacak bir değişiklik belirtilmedi."
	}
	return "Geliştirici şifresi doğrulanamadı."
}

// humanDuration renders a retry delay the way a person would say it.
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%d dakika", int(d.Minutes()+0.5))
	case d <= 0:
		return "birkaç saniye"
	default:
		return fmt.Sprintf("%d saniye", int(d.Seconds()+0.5))
	}
}

// FormField is the form field name the password is submitted under.
//
// Named here so the template and the handler cannot disagree - a
// mismatch would look exactly like a user who typed nothing, which is
// the failure that is hardest to spot because it produces a plausible
// message rather than an error.
const FormField = "developer_password"

// FromRequest reads a submitted password out of a form.
//
// It exists so that call sites do not each reach into the request
// themselves, which is where the field name drifts and where somebody
// eventually reads it from a query string - putting the developer
// password in the URL, and from there into every access log and browser
// history on the path.
//
// Only POST bodies are read, and never the URL, for exactly that
// reason.
func FromRequest(r *http.Request) string {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return ""
	}
	if err := r.ParseForm(); err != nil {
		return ""
	}
	// PostForm rather than Form: Form merges the URL query in, which is
	// the one place this value must never be honoured.
	return strings.TrimSpace(r.PostForm.Get(FormField))
}

// RequestFrom builds a gate Request from an HTTP request and a
// server-decided action list.
//
// The actions are a parameter rather than something read from the
// request on purpose. They are the server's own conclusion about what is
// being changed; a request that could name the actions it wanted
// authorized would be a request that authorizes itself, which is the
// whole failure this gate exists to prevent.
func RequestFrom(r *http.Request, actor string, actions ...string) Request {
	return Request{
		Actions:  actions,
		Password: FromRequest(r),
		Actor:    actor,
		Peer:     PeerOf(r),
	}
}

// PeerOf reports the immediate network peer of a request.
//
// Deliberately not X-Forwarded-For. This is the panel, reachable only by
// people who have already authenticated, and a forwarding header is a
// claim any of them can write. For a throttling counter and an audit
// record, a value that cannot be forged is worth more than one that is
// usually more precise.
func PeerOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
