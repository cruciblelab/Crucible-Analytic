// Package ipsources is the library of IP range datasets this build knows
// how to fetch and parse.
//
// # Why its own package
//
// It is a table with no behaviour and no dependencies, and two very
// different things need it: internal/asnlookup, which is traffic-path
// code that downloads and parses, and internal/panel, which turns it
// into the options on a settings page.
//
// The panel's registry deliberately does not import traffic-path
// packages - see the overload-policy constants in internal/panel, which
// are mirrored by hand for exactly that reason, with a test asserting
// the two lists agree. Mirroring this table would have meant the same
// arrangement: a second list, and a test to keep it honest.
//
// A leaf package removes the choice. There is one list, both sides
// import it, and the panel drags in nothing that resolves an address.
package ipsources

import "sort"

// The dataset library: which range datasets this build knows how to
// fetch and parse, and why a deployment would choose one over another.
//
// # Why a library in code rather than a URL box in the panel
//
// A source is not a URL, it is a URL *and a parser*. parse.go is written
// to two exact shapes - three columns for country, four for ASN - and
// was verified against real downloaded data rather than against a
// specification. A box in the panel could therefore only ever point at
// something already in a supported shape, which means it could not do
// the thing people would expect it to do: add a provider. Believing it
// could is worse than not having it.
//
// The one thing such a box *could* do - same format, different host - is
// a mirror, and mirrors are already served by asn_lookup.local_csv_path.
// So the box would open the whole SSRF surface for something either
// impossible or already present. PLAN.md's permanent list forbids any
// operation whose parameter is a hostname the deployment will connect
// to, and this is why.
//
// # Single source of truth
//
// The settings enums are generated from this table at init(). Adding a
// source is one entry here and the panel sees it with no second list to
// update - the property internal/panel's source suite measures by adding
// one and watching the enum grow.
//
// # Why every entry is PDDL
//
// Measured on 2026-09-01 by reading sapics/ip-location-db's own README
// and downloading each file. The same repository also publishes DB-IP
// Lite (CC BY 4.0) and GeoLite2 (MaxMind's own licence), and neither is
// here on purpose: both put an obligation on the *deployment* - an
// attribution to carry, terms to accept - and THIRD-PARTY.md's whole
// position is that a dataset this software fetches must not hand the
// customer a licence their lawyer has to read.
//
// A source that costs the deployment something can still be added. It
// would need that cost stated in Why, and in THIRD-PARTY.md, before it
// ships.

// SourceKind says which of the two datasets an entry provides.
//
// Two constants rather than a "provides both" flag: the two are fetched,
// parsed and stored independently - one failing has no effect on the
// other - and a single entry claiming both would have to be split again
// at every use.
type SourceKind int

const (
	// KindCountry is an IP → ISO 3166-1 alpha-2 country dataset.
	KindCountry SourceKind = iota
	// KindASN is an IP → autonomous system dataset.
	KindASN
)

// Source is one dataset this build can fetch and parse.
type Source struct {
	// ID is the stable identifier, and it is what a setting stores.
	//
	// Stable is the requirement: a deployment that chose one has the id
	// in its settings table, so renaming an id silently reverts that
	// deployment to the default at the next refresh.
	ID string
	// Label and Why are Turkish, because they are read in the panel.
	Label string
	// Why answers the only question a person actually has here, which is
	// not "what is this" but "why would I pick it over the other one".
	Why string

	Kind SourceKind

	// IPv4URL and IPv6URL are where the files come from.
	IPv4URL, IPv6URL string
	// IPv4File and IPv6File are the names the same files carry inside a
	// local mirror directory.
	//
	// Every source has them, because asn_lookup.local_csv_path is a
	// *transport*, not a source: it says "read these files from here
	// instead of downloading them". Modelling the mirror as its own
	// entry in this table was the first design and it was wrong - it
	// would have had to answer "which dataset is the local one", and the
	// answer is "whichever one you chose".
	IPv4File, IPv6File string

	// Licence is the terms the dataset itself is published under, as
	// read from the publisher rather than assumed.
	Licence string
}

// The library.
//
// Ordered as the panel lists them: the default first, then the
// alternatives. Not alphabetical - the first entry is the one a reader
// should take if they have no reason to choose.
var sources = []Source{
	{
		ID:    "user-country",
		Label: "Kullanıcı ülkesi (varsayılan)",
		Why: "Ziyaretçinin bulunduğu ülkeyi önceler — VPN ya da vekil üzerinden " +
			"gelse bile. Ziyaretçi coğrafyası için doğru olan bu.",
		Kind:     KindCountry,
		IPv4URL:  releaseURL + "user-country-ipv4.csv",
		IPv6URL:  releaseURL + "user-country-ipv6.csv",
		IPv4File: "user-country-ipv4.csv",
		IPv6File: "user-country-ipv6.csv",
		Licence:  licencePDDL,
	},
	{
		ID:    "server-country",
		Label: "Sunucu ülkesi",
		Why: "Adresin *barındırıldığı* ülkeyi verir, kullanıcının değil. Bir " +
			"veri merkezinden mi geliyor sorusu için doğru olan bu — D3'ün " +
			"sunucu ülkeleri kırılımı tam olarak bunu soruyor.",
		Kind:     KindCountry,
		IPv4URL:  releaseURL + "server-country-ipv4.csv",
		IPv6URL:  releaseURL + "server-country-ipv6.csv",
		IPv4File: "server-country-ipv4.csv",
		IPv6File: "server-country-ipv6.csv",
		Licence:  licencePDDL,
	},
	{
		ID:    "iptoasn-country",
		Label: "IPtoASN ülkesi",
		Why: "Bağımsız bir derleme. Yukarıdakilerle aynı biçimde ama farklı " +
			"kaynaktan üretiliyor, o yüzden yedek sıralamasında anlamlı: " +
			"birincisi kesildiğinde ikincisi de aynı anda kesilmiş olmaz.",
		Kind:     KindCountry,
		IPv4URL:  releaseURL + "iptoasn-country-ipv4.csv",
		IPv6URL:  releaseURL + "iptoasn-country-ipv6.csv",
		IPv4File: "iptoasn-country-ipv4.csv",
		IPv6File: "iptoasn-country-ipv6.csv",
		Licence:  licencePDDL,
	},
	{
		ID:    "origin-asn",
		Label: "Origin ASN (varsayılan)",
		Why: "Duyurulan yönlendirmeden türetilir ve kuruluş adını tam hâliyle " +
			"taşır (örn. \"Cloudflare, Inc.\"). Panelde okunması kolay olan bu.",
		Kind:     KindASN,
		IPv4URL:  releaseURL + "origin-asn-ipv4.csv",
		IPv6URL:  releaseURL + "origin-asn-ipv6.csv",
		IPv4File: "origin-asn-ipv4.csv",
		IPv6File: "origin-asn-ipv6.csv",
		Licence:  licencePDDL,
	},
	{
		ID:    "iptoasn-asn",
		Label: "IPtoASN ASN",
		Why: "Bağımsız derleme, aynı biçim. Adları ağ kısaltmasıyla verir " +
			"(\"CLOUDFLARENET\"), yani insan için daha az okunaklı ama " +
			"eşleştirme için daha kararlı. Yedek olarak anlamlı.",
		Kind:     KindASN,
		IPv4URL:  releaseURL + "iptoasn-asn-ipv4.csv",
		IPv6URL:  releaseURL + "iptoasn-asn-ipv6.csv",
		IPv4File: "iptoasn-asn-ipv4.csv",
		IPv6File: "iptoasn-asn-ipv6.csv",
		Licence:  licencePDDL,
	},
}

const (
	// releaseURL is the one host this build downloads datasets from.
	//
	// A constant rather than five copies, so the answer to "where does
	// this deployment connect to" is one line - which is the question a
	// customer's security review asks first.
	releaseURL = "https://github.com/sapics/ip-location-db/releases/download/latest/"

	// licencePDDL is the Public Domain Dedication and License v1.0.
	//
	// Free use, no attribution required. Read from the publisher's own
	// README on 2026-09-01, per dataset, rather than assumed from the
	// repository's code licence - they are different things and the
	// same repository publishes datasets under three different sets of
	// terms.
	licencePDDL = "PDDL 1.0 — kamu malı, atıf gerekmiyor"
)

// DefaultCountry and DefaultASN are what a deployment that
// has chosen nothing uses.
//
// Named constants rather than "sources[0]", because the defaults are a
// compatibility promise: they are what every existing installation is
// already fetching, and reordering this table must not silently change
// what those installations download.
const (
	DefaultCountry = "user-country"
	DefaultASN     = "origin-asn"
)

// ByID returns one entry.
func ByID(id string) (Source, bool) {
	for _, s := range sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

// IDs lists the ids of one kind, in library order.
//
// This is what generates the settings enum. Order is the table's, not
// sorted, so the default stays first in the panel's dropdown.
func IDs(kind SourceKind) []string {
	var out []string
	for _, s := range sources {
		if s.Kind == kind {
			out = append(out, s.ID)
		}
	}
	return out
}

// All returns every entry, for the panel and for tests.
func All() []Source {
	out := make([]Source, len(sources))
	copy(out, sources)
	return out
}

// SortedIDs is every id of both kinds, sorted.
//
// For the fallback-order setting, whose value is a list and whose
// admissible members are "any source at all" - a country fallback list
// naming an ASN dataset is a mistake worth reporting, and reporting it
// needs the full set to compare against.
func SortedIDs() []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}
