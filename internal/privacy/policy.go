package privacy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The two operator-supplied facts on the disclosure, and the one rule
// that decides whether they may be shown.
//
// # Why they are checked in this package rather than at the form
//
// Because there are two callers and only one question. The panel checks
// what a customer typed, so it can refuse it with a sentence; the beacon
// checks what it reads out of the database, so it can decline to render
// it. A rule that lived only in the form would be a rule that a row
// edited by hand walks straight past - and this value ends up in an
// href on a page served to the public.
//
// *İstemciye güvenme, sadece sunucuya güven* applies to our own database
// too: the panel is a client of the beacon, and the row between them is
// input.
//
// # What is refused, and why each one
//
//   - Anything but http and https. `javascript:` in an href is the
//     oldest cross-site scripting vector there is; html/template would
//     neutralise it on our page, but the same string also reaches a
//     customer's own page through the JSON endpoint, where nothing of
//     ours is escaping anything.
//   - Credentials in the URL. A link with a password in it, printed on a
//     public page, is a password published.
//   - Anything with no host. A relative path is meaningless on a page
//     that is framed from another origin.
//   - Control characters and invalid UTF-8, which are how an attacker
//     splits a header or breaks out of an attribute.

// MaxPolicyURL and MaxContact bound what may be stored.
//
// Not arbitrary: browsers and mail servers stop honouring longer values
// anyway, so a longer one is either a mistake or somebody using a
// settings row as storage.
const (
	MaxPolicyURL = 2000
	MaxContact   = 254
)

// ErrEmpty is what the two functions return for a blank value, so a
// caller can tell "nothing set" from "refused".
//
// Both callers treat it as "no link" rather than as a fault: an empty
// setting is the default state of every deployment, and a panel that
// showed an error on a form nobody had filled in yet would be teaching
// its customer to ignore errors.
var ErrEmpty = errors.New("privacy: nothing set")

// CleanPolicyURL canonicalises the customer's own policy page address.
//
// Returns ErrEmpty for a blank value. Any other error is a value that
// must not be shown to a visitor, and the message is Turkish because
// the panel prints it under the field.
func CleanPolicyURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrEmpty
	}
	if err := printableUTF8(trimmed); err != nil {
		return "", err
	}
	if len(trimmed) > MaxPolicyURL {
		return "", fmt.Errorf("adres %d karakteri aşıyor", MaxPolicyURL)
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("adres okunamadı; https://site.example/gizlilik gibi tam bir adres yazın")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return "", errors.New("adres http:// ya da https:// ile başlamalı")
	default:
		return "", fmt.Errorf("%q adresleri kabul edilmiyor; yalnız http ve https", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("adreste alan adı yok; https://site.example/gizlilik gibi olmalı")
	}
	if u.User != nil {
		return "", errors.New("adreste kullanıcı adı ya da parola var; herkese açık bir sayfada gösterilecek")
	}
	return u.String(), nil
}

// CleanContact canonicalises where a visitor's request should go.
//
// An address or a page: a deployment that handles requests by email
// gives an address, one that has a form gives its URL. Both are printed
// on a public page, so both go through the same refusals - and the mail
// address gets one more, because "a@b" is a valid mailbox on a local
// network and a dead end on the internet.
func CleanContact(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrEmpty
	}
	if err := printableUTF8(trimmed); err != nil {
		return "", err
	}
	if len(trimmed) > MaxContact {
		return "", fmt.Errorf("iletişim adresi %d karakteri aşıyor", MaxContact)
	}

	// A scheme means it is meant as a link. mailto: is spelled out
	// rather than accepted as "some scheme", because the whole point of
	// the list is that it is a list.
	if lower := strings.ToLower(trimmed); strings.Contains(lower, "://") || strings.HasPrefix(lower, "mailto:") {
		if strings.HasPrefix(lower, "mailto:") {
			address, err := cleanMailbox(trimmed[len("mailto:"):])
			if err != nil {
				return "", err
			}
			return address, nil
		}
		return CleanPolicyURL(trimmed)
	}
	return cleanMailbox(trimmed)
}

// cleanMailbox checks a bare email address.
//
// Deliberately not RFC 5322: that grammar admits quoted strings,
// comments and nested folding that no customer will type and that this
// project has no business rendering. What is checked is what a wrong
// value looks like - a missing part, a space, a domain with no dot.
func cleanMailbox(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if address == "" {
		return "", ErrEmpty
	}
	if strings.ContainsAny(address, " \t<>\",;") {
		return "", errors.New("e-posta adresinde boşluk ya da ayraç var; yalnız adresi yazın")
	}
	local, domain, ok := strings.Cut(address, "@")
	if !ok || strings.Contains(domain, "@") {
		return "", errors.New("e-posta adresinde tam olarak bir @ olmalı")
	}
	if local == "" || domain == "" {
		return "", errors.New("e-posta adresinin iki yanı da dolu olmalı")
	}
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", errors.New("alan adı bir nokta içermeli; ornek@site.example gibi")
	}
	return address, nil
}

// printableUTF8 refuses what must never reach an attribute or a header.
func printableUTF8(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("değer geçerli metin değil")
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			return errors.New("değer satır sonu ya da sekme içeriyor")
		}
		if unicode.IsControl(r) {
			return errors.New("değer görünmeyen bir denetim karakteri içeriyor")
		}
	}
	return nil
}

// ShownPolicyURL is the address a page may link to, or empty.
//
// The reading half of CleanPolicyURL: it answers "may this be rendered"
// without an error, because the renderer has nobody to show an error to.
// A value that fails is dropped, which is the completion criterion P3
// states in as many words - *bozuk bağlantı göstermiyor*.
func ShownPolicyURL(raw string) string {
	clean, err := CleanPolicyURL(raw)
	if err != nil {
		return ""
	}
	return clean
}

// ShownContact is the same for the contact address.
func ShownContact(raw string) string {
	clean, err := CleanContact(raw)
	if err != nil {
		return ""
	}
	return clean
}

// ContactIsMailbox says whether a contact should be linked as mailto:.
//
// Asked of the cleaned value, so a caller cannot be handed something
// that failed the checks and then have to guess what it is.
func ContactIsMailbox(clean string) bool {
	return clean != "" && !strings.Contains(strings.ToLower(clean), "://")
}
