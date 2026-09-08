package beacon

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// The two disclosure endpoints.
//
// # Why the beacon serves them
//
// It is the only service a visitor can reach. docker/compose.yml
// publishes no port for the panel or the read API and says why in as
// many words; a disclosure behind a login form is one nobody opens.
//
// # Why there are two
//
// They answer for two different readers. The JSON is for a customer
// printing this on their own privacy page, in their own language and
// their own design - so it carries facts and no prose, because prose
// would have to be translated, which means retyped, which means a copy
// that stops agreeing with the setting. The HTML is for a deployment
// that wants a page today and will write their own later.
//
// # What makes the content correct rather than merely present
//
// Every claim comes from something the code does. The mode is read from
// the same live atomic the writer reads, so a change on the panel shows
// up on the next request. The stored field names come from the writer's
// own column list rather than from a list typed here - a second list
// would be right today and wrong after the first migration, and it
// would be wrong in the direction that matters: telling a visitor less
// is collected than is.

// privacyResponse is what GET <prefix>/privacy answers.
//
// privacy.Notice plus the one thing that package cannot know: which
// columns this build writes. Embedded rather than nested so a customer's
// template reaches every field without a level of indirection nobody
// needs.
type privacyResponse struct {
	privacy.Notice
	// Stored is every column a beacon event writes, sorted.
	//
	// From writer.columns, which is the list CopyFrom actually uses.
	// Sorted rather than left in insert order because insert order is
	// an implementation detail and a reader comparing two deployments
	// should not see a diff that means nothing.
	Stored []string `json:"stored"`
	// Site is which site this beacon accepts events for, when it is one.
	//
	// Named because a visitor reading this on a shared host has a right
	// to know which property it describes. Empty when the beacon serves
	// several, rather than listing them: the list is the customer's
	// business and a visitor did not ask which other sites exist.
	Site string `json:"site,omitempty"`
}

// RotatesHuman is the rotation period in words, for the page.
//
// A method on the response rather than a field on privacy.Notice: the
// JSON carries the number of seconds, which is what a customer's own
// template wants, and turning it into a phrase is a rendering decision
// that belongs beside the rendering.
//
// Only the shapes this product actually configures are given words.
// Anything else falls back to the duration's own form, which is ugly and
// correct - a page that guessed "her 36 saatte bir" as "her gün" would
// be a disclosure that rounds.
func (p privacyResponse) RotatesHuman() string {
	switch d := p.IdentifierRotatesEvery; {
	case d == 24*time.Hour:
		return "günde"
	case d%(24*time.Hour) == 0:
		return "her " + strconv.Itoa(int(d/(24*time.Hour))) + " günde"
	case d%time.Hour == 0:
		return "her " + strconv.Itoa(int(d/time.Hour)) + " saatte"
	default:
		return "her " + d.String() + "'de"
	}
}

// privacyNotice builds the answer from what this process is doing now.
func (s *Server) privacyNotice() privacyResponse {
	stored := make([]string, len(columns))
	copy(stored, columns)
	sort.Strings(stored)

	out := privacyResponse{
		Notice: privacy.NewNotice(s.ipMode(), s.visitors().period()),
		Stored: stored,
	}
	if len(s.Sites) == 1 {
		out.Site = s.Sites[0]
	}
	return out
}

func (s *Server) handlePrivacyJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Readable from a customer's own page, which is the entire point of
	// this endpoint: their privacy page is on their origin and this is
	// on the beacon's. No credentials are involved and nothing here is
	// visitor-specific, so the open header costs nothing.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Short rather than long. The mode can change while the process
	// runs, and a disclosure cached for an hour would keep telling
	// visitors about a setting the customer changed on the panel five
	// minutes ago.
	w.Header().Set("Cache-Control", "public, max-age=60")

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.privacyNotice()); err != nil {
		s.logger().Warn("beacon: writing the privacy notice", "err", err)
	}
}

func (s *Server) handlePrivacyPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	// No inline script and no external anything, so this renders under
	// the strictest policy a customer is likely to have - and under one
	// they have not thought about. A disclosure page that needs a CSP
	// exemption is a page that will be reported broken rather than read.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")

	if err := privacyPage.Execute(w, s.privacyNotice()); err != nil {
		s.logger().Warn("beacon: rendering the privacy page", "err", err)
	}
}

// privacyPage is the ready-made page.
//
// # Why the prose is here and only here
//
// Because this is the one place it is allowed to be. The facts live in
// internal/privacy, this turns them into sentences, and nothing else in
// the tree writes a second version - an invariant checks that. A copy in
// a README or a panel page would be the copy that keeps saying "masked"
// after somebody switches to full.
//
// # Turkish only, deliberately
//
// The beacon has no idea what language a visitor reads and no
// negotiation to find out, and guessing from Accept-Language would make
// this page say different things to two people looking at the same site.
// A customer serving another language uses the JSON endpoint, which is
// what it is for. Saying that here, on the page, so nobody has to guess
// whether it was an oversight.
var privacyPage = template.Must(template.New("privacy").Parse(`<!doctype html>
<html lang="tr"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Bu sitede ne ölçülüyor</title>
<style>
body{font:16px/1.6 system-ui,sans-serif;max-width:42rem;margin:2rem auto;padding:0 1rem;color:#111}
h1{font-size:1.4rem} h2{font-size:1.1rem;margin-top:2rem}
code{background:#f2f2f2;padding:.1em .3em;border-radius:3px}
ul{padding-left:1.2rem} .not{color:#444}
</style></head><body>
<h1>Bu sitede ne ölçülüyor{{if .Site}} — {{.Site}}{{end}}</h1>

<p>Bu site ziyaretçi sayımı için Crucible Analytic kullanıyor. Aşağıdakiler
bu kurulumun <em>şu anki ayarından</em> türetilmiştir; ayar değişirse bu
sayfa da değişir.</p>

<h2>Adresiniz</h2>
<p>Ham IP adresiniz hiçbir zaman kaydedilmiyor. Kaydedilen, adresin ağ
kısmı: <code>{{.AddressMaskedTo}}</code>.</p>
{{if .TokenFromWholeAddress}}
<p>Bu kurulum ayrıca adresin tamamından türetilmiş, anahtarla üretilen bir
<em>jeton</em> saklıyor. Adresin kendisi yine yazılmıyor; jetonun eklediği
şey, aynı ağ içindeki iki ziyaretçiyi birbirinden ayırabilmek.</p>
{{else}}
<p>Aynı ağın arkasındaki ziyaretçiler birbirinden ayrılmıyor: aynı ofisten
ya da aynı operatörden gelen iki kişi bu kurulumda aynı görünür.</p>
{{end}}

<h2>Çerez yok</h2>
<p>Bu ölçüm hiçbir çerez kullanmıyor. Ziyaretçi kimliği, sunucunun
belleğinde tuttuğu ve <strong>{{.RotatesHuman}}</strong> bir kez
yenilediği bir sırdan türetiliyor. O sır hiçbir yere yazılmıyor ve
yenilendiğinde kayboluyor.</p>

<h2>Ne saklanıyor</h2>
<ul>{{range .Stored}}<li><code>{{.}}</code></li>{{end}}</ul>

<h2>Silme talebi neden yok</h2>
<p class="not">Bu sistemde size ait bir kayıt kümesini gösterebilecek
kalıcı bir kimlik tutulmuyor. Günün sırrı yenilendikten sonra hangi
satırların sizin olduğunu <em>kimse</em> gösteremez — bu kurulumu işleten
kişi de gösteremez. Yani teslim edilecek bir küme yok, silinecek bir küme
de yok.</p>
<p class="not">Bunu tersine çevirmenin tek yolu daha fazlasını toplamak
olurdu: sizi tanıyabilmek için sizi tanıyan bir şey saklamak. Bu tasarım
onu yapmıyor.</p>

<h2>Sayılmak istemiyorsanız</h2>
<p>Tarayıcınızın konsolunda <code>{{.OptOut}}</code> çalıştırın. Seçim bu
tarayıcıda saklanır ve bir daha hiçbir şey gönderilmez. Geri açmak için
<code>window.crucible.optIn()</code>.</p>

<h2>Bu sayfanın makine okunabilir hâli</h2>
<p>Aynı bilgiler <code>privacy</code> ucundan JSON olarak da alınabilir.
Bu sayfa yalnız Türkçe; başka bir dilde yayımlamak isteyen site sahibi
JSON'u kendi sayfasında kendi diliyle basar.</p>
</body></html>
`))
