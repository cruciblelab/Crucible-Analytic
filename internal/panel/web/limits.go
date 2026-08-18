package web

import (
	"errors"
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Request size limits.
//
// Go's ParseForm reads an entire urlencoded body into memory with no cap
// of its own - only multipart bodies get one, through
// ParseMultipartForm. So without this, every POST in this panel was an
// unbounded allocation, reachable by anybody, before authentication: a
// 64 MiB body cost about 128 MiB of heap and was then rejected for a
// missing CSRF token, having already been paid for. A handful of
// concurrent ones takes the process out.
//
// That is CWE-770, and it is the kind of hole that survives review
// because nothing in the handler looks wrong - the missing thing is a
// line nobody wrote.

// maxFormBytes caps a form submission.
//
// Two orders of magnitude above the largest form here (the wizard's site
// list, a textarea of hostnames) and small enough that flooding this
// endpoint costs the sender more than it costs the server. A panel form
// that legitimately needs more than a quarter of a megabyte does not
// exist and should be reviewed rather than accommodated.
const maxFormBytes = 256 << 10

// haveStore refuses a request that would read the database when there is
// no database.
//
// The routes the internet can reach without credentials - the sign-in
// form, an invitation link, a developer link, the first-run page - all
// read a row before they know who is asking, and each one dereferencing
// a nil Store is a remote crash rather than an error page.
//
// ListenAndServe refuses to start without a Store, so this state should
// never exist in a running deployment. It is checked anyway because
// "unreachable" and "takes the whole process down" are a bad pair of
// properties to hold at the same time, and because the thing making it
// unreachable is one line in another file.
//
// Called by the handlers that need it rather than wrapped around the
// tree: the static assets deliberately do not need a database, and a
// panel that cannot serve its own stylesheet renders its 503 page as
// unstyled text.
//
// 503 rather than 500: nothing is wrong with the request, and from the
// caller's side the condition is temporary.
func (s *Server) haveStore(w http.ResponseWriter, r *http.Request, lang *ui.Language) bool {
	if s.Store != nil {
		return true
	}
	s.Renderer.ErrorIn(w, r, http.StatusServiceUnavailable, lang)
	return false
}

// LimitRequestBodies caps every request body reaching the panel.
//
// Applied as middleware rather than in each handler, because "every
// handler remembers" is exactly the property that fails: the eight
// ParseForm calls in this package were written over three phases by
// somebody who was thinking about something else each time.
//
// MaxBytesReader also arranges for the connection to be closed once the
// limit is hit, so a client streaming an endless body is disconnected
// rather than politely read to completion.
func LimitRequestBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// acceptPost is the gate every state-changing handler goes through:
// read the form, then check the token.
//
// The order is the point. CheckCSRF reads a form field, which makes it
// the thing that first touches the body - so a body over the limit used
// to surface as "your CSRF token is stale", sending whoever is debugging
// to reload a page that will fail again the same way. Parsing first
// means the size is reported as a size.
//
// It is also the honest order on its own terms: a token cannot be
// checked in a form that has not been parsed.
func (s *Server) acceptPost(w http.ResponseWriter, r *http.Request, lang *ui.Language) bool {
	if !s.parseForm(w, r, lang) {
		return false
	}
	if !s.Sessions.CheckCSRF(r) {
		// 419 rather than 403: the fix is "reload and try again", not
		// "you may not". A form left open while somebody went to run a
		// command on the server is the ordinary way to get here.
		s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
		return false
	}
	return true
}

// parseForm reads a form and answers oversized ones honestly.
//
// Every handler in this package goes through here rather than calling
// r.ParseForm directly, so the size limit has one place to be reported
// from - and so a body that was refused for being too large does not
// arrive as "bad request", which sends whoever is debugging to look at
// their field names.
func (s *Server) parseForm(w http.ResponseWriter, r *http.Request, lang *ui.Language) bool {
	err := r.ParseForm()
	if err == nil {
		return true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		// 413 rather than 400. The request was well-formed; there was
		// just too much of it, and that is a different thing to fix.
		s.Renderer.ErrorIn(w, r, http.StatusRequestEntityTooLarge, lang)
		return false
	}
	s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
	return false
}
