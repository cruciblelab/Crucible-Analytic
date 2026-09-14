package beacon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
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

// EffectiveSinceHuman is the day the mode's setting was last written, as
// a date rather than a timestamp.
//
// A day because the hour is not the point, and printing it would invite
// a visitor to compare clocks with a server. In UTC because the JSON
// beside it is: two encodings of one instant that disagreed about the
// day would be worse than either alone.
//
// # Why digits and not "14 Mart 2026"
//
// Month names are in the panel's language pack, which this binary does
// not have and must not reach into - the beacon is the public-facing
// service and the pack is a file the panel loads. Spelling twelve names
// again here would be a second copy of a list nothing compares, and the
// numeric form is the same one the Turkish pack already uses for its
// short date (kisa_tarih = "02.01.2006").
//
// Empty when there is no date, and the template's branch on this is
// what keeps the sentence off a page that has nothing to put in it.
func (p privacyResponse) EffectiveSinceHuman() string {
	if p.EffectiveSince.IsZero() {
		return ""
	}
	return p.EffectiveSince.UTC().Format("02.01.2006")
}

// NoteFor glosses a stored column name that a visitor would otherwise
// read as more than it holds.
//
// # The defect this fixes
//
// The stored list is the writer's own column list, which is the property
// worth keeping: a second list typed here would tell a visitor less is
// collected than is. But column names are the database's words, not a
// visitor's, and one of them is `ip`. The page said "your raw IP address
// is never recorded" and then, three sections down, listed a field
// called `ip` - a contradiction on a page whose whole purpose is not to
// be misread. Found by looking at a screenshot; no assertion could have
// found it, because every fact on the page was true.
//
// What that column holds is the masked address (storedAddress →
// privacy.MaskIP), and the prefix lengths come from the same string the
// page prints above, so the gloss cannot claim a mask the code does not
// apply.
//
// # And ip_hash in masked mode
//
// Empty. storedIPHash returns nil unless the mode tokenises, so in
// masked mode the column exists and every row's value is null. That is
// less than the list suggests, which is the harmless direction - but a
// disclosure that can say so should.
//
// Returns "" for every other column: a gloss on all thirty names would
// be a second description of the product in a place nothing compares,
// and these two are the ones that read as something they are not.
func (p privacyResponse) NoteFor(column string) string {
	switch column {
	case colIP:
		return "adresin yalnız ağ kısmı (" + p.AddressMaskedTo + "), tamamı değil"
	case colIPHash:
		if p.TokenFromWholeAddress {
			return "yukarıda anlatılan jeton; adresin kendisi değil"
		}
		return "bu kipte hiç yazılmıyor, boş kalıyor"
	}
	return ""
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

	// The two operator-supplied facts, checked here rather than trusted.
	//
	// They arrive from a settings row, which the panel validates on the
	// way in - and a row is still a place a value can come from without
	// passing that form: an older build, a hand-edited database, a
	// restore from a backup taken before the check existed. This page is
	// served to the public and this value ends up in an href, so it is
	// checked at the point of use as well. A value that fails is
	// dropped, which is P3's own criterion: *bozuk bağlantı
	// göstermiyor*.
	d := s.disclosure()
	out.PolicyURL = privacy.ShownPolicyURL(d.PolicyURL)
	out.Contact = privacy.ShownContact(d.Contact)
	out.EffectiveSince = d.ModeEffectiveSince
	return out
}

// ContactHref is the contact address as a link target.
//
// A method rather than a second stored field: what is stored is the
// address, and whether it is written as mailto: is a rendering decision
// that belongs beside the rendering - the same reasoning as
// RotatesHuman.
func (p privacyResponse) ContactHref() string {
	if privacy.ContactIsMailbox(p.Contact) {
		return "mailto:" + p.Contact
	}
	return p.Contact
}

// surfaceOff answers a request for a disclosure this deployment has
// switched off.
//
// 404 rather than 403: to a visitor there is no such page here, which is
// the truth. The CORS header goes out with it on purpose - beacon.js
// asks this endpoint whether to draw the embedded block, and a 404 that
// a cross-origin script cannot read is a 404 that turns into a console
// error instead of an answer.
func (s *Server) surfaceOff(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) handlePrivacyJSON(w http.ResponseWriter, r *http.Request) {
	if !s.disclosure().Enabled {
		s.surfaceOff(w)
		return
	}
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

	notice := s.privacyNotice()

	// A validator, so a customer's own privacy page can ask "has this
	// changed" without re-reading and re-diffing the whole notice.
	//
	// # Why this endpoint of all of them
	//
	// Because it is the one a *consumer* polls. The ready-made page is
	// for a visitor, who reads it once; this JSON is for a site that
	// prints the same facts in its own language and design, and that
	// site has to notice a mode escalation - more personal data from
	// the next request on - without being told by a person.
	//
	// Last-Modified is the date the mode came into force, so a
	// conditional GET is one comparison rather than a body and a diff.
	// ETag covers everything else: the operator's policy URL and
	// contact can change without the mode moving, and a validator that
	// ignored them would answer 304 to a consumer whose page is now
	// showing a dead link.
	//
	// # Why the tag is derived rather than counted
	//
	// It is a hash of the encoded notice. A version number would be a
	// second piece of state to keep equal to the first, and this
	// project's own rule is that two copies are acceptable only when
	// something compares them. The body is the thing; its hash cannot
	// disagree with it.
	body, err := json.MarshalIndent(notice, "", "  ")
	if err != nil {
		s.logger().Warn("beacon: encoding the privacy notice", "err", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	if !notice.EffectiveSince.IsZero() {
		w.Header().Set("Last-Modified", notice.EffectiveSince.UTC().Format(http.TimeFormat))
	}
	// Vary is not set and that is deliberate: nothing here varies by
	// request header. Saying otherwise would tell a cache to keep as
	// many copies as it sees header combinations, for one answer.
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if _, err := w.Write(append(body, '\n')); err != nil {
		s.logger().Warn("beacon: writing the privacy notice", "err", err)
	}
}

// etagMatches compares an If-None-Match header against this answer's tag.
//
// The header may carry a list, and each entry may be weak (W/ prefix).
// Handled rather than compared whole, because a consumer sending two
// tags - the ordinary shape after a redeploy - would otherwise always
// get a full body, which is the thing the validator exists to avoid.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (s *Server) handlePrivacyPage(w http.ResponseWriter, r *http.Request) {
	if !s.disclosure().Enabled {
		s.surfaceOff(w)
		return
	}
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

<p class="not"><strong>Bu bir gizlilik politikası değil.</strong> Burada
yazanlar yalnız ölçüm aracının ne kaydettiğini anlatan, ayarlardan
otomatik üretilmiş teknik bir özet. Hukuki bir metin değil ve hukuki
tavsiye yerine geçmez — bu yazılımı yazanlar hukuk danışmanı değil.
Sitenin kendi politikası ve onunla ilgili sorumluluk siteyi işleten
kişiye ait.</p>

{{if .TokenFromWholeAddress}}
<p><strong>Bu kurulum, iki kipten çok saklayanında:</strong> aynı ağın
arkasındaki iki ziyaretçi birbirinden ayrılabiliyor, diğer kipte
ayrılamıyor. Ham adresiniz yine kaydedilmiyor; nasıl olduğu aşağıda,
&#8220;Adresiniz&#8221; başlığı altında.</p>
{{end}}

{{with .EffectiveSinceHuman}}
<p><strong>Adresin nasıl saklandığına dair ayar en son {{.}} tarihinde
yazıldı</strong> (UTC). O tarihten önce okuduysanız, okuduğunuz metin
bundan farklı olabilir. Hangi yöne değiştiğini bu sayfa söylemez: bu
servis ayarın önceki değerini görmüyor.</p>
{{end}}

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
<p class="not">Aşağıdakiler veritabanı sütunlarının adları — bu kurulumun
yazdığı alanların listesi, elle yazılmış bir özet değil. Adları teknik
olduğu için, adresle ilgili ikisinin ne tuttuğu yanlarında yazıyor.</p>
<ul>{{range .Stored}}<li><code>{{.}}</code>{{with $.NoteFor .}} — {{.}}{{end}}</li>{{end}}</ul>

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

<h2>Bu sitenin kendi gizlilik metni</h2>
{{if .PolicyURL}}
<p>Yukarıdakiler ölçümün kendisi hakkında. Bu sitenin işleten kişinin
kendi yazdığı gizlilik metni ayrı bir sayfada:
<a href="{{.PolicyURL}}">{{.PolicyURL}}</a></p>
{{else}}
<p class="not">Bu kurulum ayrı bir gizlilik metni adresi tanımlamamış.
Yokluğu burada yazıyor, çünkü bu sayfanın onun yerine geçtiği
sanılmasın.</p>
{{end}}
{{if .Contact}}
<h2>Soru sormak isterseniz</h2>
<p>Bu kurulumu işleten kişiye şuradan ulaşabilirsiniz:
<a href="{{.ContactHref}}">{{.Contact}}</a>. Yukarıdaki "silinecek bir küme
yok" cümlesi teknik bir sonuç; başka bir sorunuz varsa muhatabınız bu
adres.</p>
{{end}}

<h2>Bu sayfanın makine okunabilir hâli</h2>
<p>Aynı bilgiler <code>privacy</code> ucundan JSON olarak da alınabilir.
Bu sayfa yalnız Türkçe; başka bir dilde yayımlamak isteyen site sahibi
JSON'u kendi sayfasında kendi diliyle basar.</p>
</body></html>
`))
