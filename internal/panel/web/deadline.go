package web

import (
	"html"
	"net/http"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/deadline"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// withDeadline answers any request still running when writeTimeout is
// nearly spent. See internal/deadline for the measurement that made this
// necessary: a page reading a table a schema upgrade had locked sent 0
// bytes after 74 seconds, and the access log wrote status=200 for it.
//
// It sits inside requestLog, not outside, and the order is the fix for
// the second half of that measurement. The access log records the status
// written through it; placed inside, the 503 this writes is that status.
// Placed outside, the log would go on recording the handler's intention.
func (s *Server) withDeadline(next http.Handler) http.Handler {
	return deadline.Handler(next, writeTimeout, s.timeoutAnswer(), s.logger)
}

// timeoutAnswer is the page, and the rule for how its log line names the
// request: through loggedPath, like the access log, because a request
// that runs out of time can be an invitation link.
func (s *Server) timeoutAnswer() deadline.Answer {
	a := timeoutPage(s.Renderer.Catalogs(), s.Language)
	a.LogPath = loggedPath
	return a
}

// timeoutPage is the one page TimeoutHandler can send: a fixed body,
// written without a handler, so it cannot be negotiated per request or
// drawn with the layout (which needs a request, a session and a
// language). It carries every language the panel does, the configured
// one first, each in its own section - a reader finds theirs, and a new
// catalog file reaches this page the same way it reaches every other.
//
// Deliberately plain: no stylesheet, no script, nothing the panel's
// content security policy would have to allow.
func timeoutPage(cats *ui.Catalogs, preferred string) deadline.Answer {
	langs := cats.Languages()
	ordered := make([]*ui.Language, 0, len(langs))
	if first := cats.ByCode(preferred); first != nil {
		ordered = append(ordered, first)
	}
	for _, l := range langs {
		if l.Code != preferred {
			ordered = append(ordered, l)
		}
	}
	if len(ordered) == 0 {
		ordered = append(ordered, cats.Base())
	}

	limit := deadline.For(writeTimeout).String()
	var b strings.Builder
	first := ordered[0]
	b.WriteString(`<!doctype html><html lang="` + html.EscapeString(first.Code) + `" dir="` +
		html.EscapeString(first.Dir) + `"><head><meta charset="utf-8"><title>` +
		html.EscapeString(first.T("hata.zamanasimi.baslik")) + "</title></head><body>\n")
	for _, l := range ordered {
		b.WriteString(`<section lang="` + html.EscapeString(l.Code) + `" dir="` + html.EscapeString(l.Dir) + `">` +
			"<h1>" + html.EscapeString(l.T("hata.zamanasimi.baslik")) + "</h1>" +
			"<p>" + html.EscapeString(l.Tf("hata.zamanasimi.govde", limit)) + "</p></section>\n")
	}
	b.WriteString("</body></html>\n")
	return deadline.Answer{ContentType: "text/html; charset=utf-8", Body: b.String()}
}
