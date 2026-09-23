package web

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The page a timed-out request gets is built once from the catalogs, so
// what is asked here is what a reader in each language finds: their own
// sentence, the deadline in it, and the configured language first.
// Asked for two configured languages, because with one the order check
// passes on a page that ignores the setting.
func TestTheTimeoutPageCarriesEveryLanguageConfiguredFirst(t *testing.T) {
	cats, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cats.Languages()) < 2 {
		t.Fatalf("%d catalogs loaded; this test needs two to say anything about order",
			len(cats.Languages()))
	}

	for _, preferred := range []string{"tr", "en"} {
		page := timeoutPage(cats, preferred)
		if page.ContentType != "text/html; charset=utf-8" {
			t.Errorf("[%s] content type = %q", preferred, page.ContentType)
		}
		body := page.Body

		first := strings.Index(body, `<section lang="`)
		if first < 0 || !strings.HasPrefix(body[first:], `<section lang="`+preferred+`"`) {
			t.Errorf("[%s] the first section is not the configured language:\n%s", preferred, body)
		}
		if !strings.Contains(body, `<html lang="`+preferred+`"`) {
			t.Errorf("[%s] the document's own lang is not the configured language", preferred)
		}

		for _, l := range cats.Languages() {
			title := l.T("hata.zamanasimi.baslik")
			if !strings.Contains(body, title) {
				t.Errorf("[%s] the %s title %q is not on the page", preferred, l.Code, title)
			}
			if !strings.Contains(body, `<section lang="`+l.Code+`"`) {
				t.Errorf("[%s] no section for %s", preferred, l.Code)
			}
		}
		// The deadline the reader is told is the one the server enforces:
		// deadline.For(60s). Spelled out, not computed.
		if strings.Count(body, "55s") != len(cats.Languages()) {
			t.Errorf("[%s] want the deadline 55s once per language:\n%s", preferred, body)
		}
	}
}
