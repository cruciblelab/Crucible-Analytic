package ui

import (
	"context"
	"net/http"
)

// langContextKey carries the resolved language on a request.
//
// Its own unexported type so nothing outside this package can collide
// with it or, worse, set it - the language on a request is decided by
// one middleware and read everywhere else.
type langContextKey struct{}

// WithLanguage returns a request carrying lang.
func WithLanguage(r *http.Request, lang *Language) *http.Request {
	if r == nil || lang == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), langContextKey{}, lang))
}

// LanguageFrom reads the language off a request, or nil.
func LanguageFrom(r *http.Request) *Language {
	if r == nil {
		return nil
	}
	lang, _ := r.Context().Value(langContextKey{}).(*Language)
	return lang
}

// Negotiate resolves the language for one request and attaches it.
//
// The order is deliberate and is the order of how deliberate each
// signal is:
//
//  1. preferred - a choice somebody actually made. Today that is the
//     deployment's configured default; when accounts gain a language
//     preference it goes in front of it, and nothing else here changes.
//  2. Accept-Language - what the reader's browser says. This is what
//     serves a colleague who reads English on a deployment configured
//     for Turkish, and it is the only signal available on the login
//     page, where nobody has an account yet.
//  3. The base language, for anything unservable.
//
// Note the panel deliberately does *not* offer a language switch in the
// URL. A `?lang=` parameter is a page that renders differently for the
// same address, which makes every screenshot in a support ticket
// ambiguous. The browser already carries this preference and accounts
// will carry it explicitly.
func (c *Catalogs) Negotiate(r *http.Request, preferred ...string) *Language {
	if r == nil {
		return c.Base()
	}
	return c.Match(r.Header.Get("Accept-Language"), preferred...)
}

// LanguageMiddleware resolves the language once per request and puts it
// on the context, so no handler has to remember to.
func LanguageMiddleware(cats *Catalogs, preferred string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, WithLanguage(r, cats.Negotiate(r, preferred)))
	})
}
