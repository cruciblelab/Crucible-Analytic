package ui

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy is the whole policy, written out so it can be
// read in one go.
//
// Two absences are the point of it:
//
//   - no 'unsafe-inline'. There is not one inline <script> or <style>
//     in this package, and there must not be, because the moment one
//     appears the policy has to be loosened for every page. A
//     structural test enforces it rather than a reviewer remembering.
//   - no 'unsafe-eval'. htmx needs it for two features - `hx-on:`
//     handlers and `js:`-prefixed hx-vals - and this panel uses
//     neither. The same test refuses templates that reach for them, so
//     the policy cannot quietly become the thing that has to give.
//
// default-src 'none' means anything not named below is denied, so a
// subresource type added later fails loudly instead of inheriting a
// permissive default.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// SecurityHeaders sets the response headers that apply to everything
// the panel serves, assets included.
//
// hsts should be true only when the panel is genuinely reached over
// HTTPS - directly, or through a proxy that terminates TLS. Sending it
// from a deployment that is reachable over plain HTTP would lock a
// browser out of a panel that has no HTTPS to fall back to, and this is
// exactly the kind of software somebody runs on a spare machine first
// and puts a certificate on later.
func SecurityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		// frame-ancestors above already says this to every browser that
		// reads CSP; X-Frame-Options is here for the ones that do not.
		h.Set("X-Frame-Options", "DENY")
		// same-origin rather than no-referrer: the panel's own links
		// benefit from it and nothing here is ever linked outward.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=(), usb=()")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// noStore is set on every rendered page, and on nothing else.
//
// Panel HTML carries the customer's numbers and a CSRF token tied to
// one session. A shared cache holding either would be the kind of leak
// that shows one customer another's page, and the browser's own
// back-button cache holding it means a logged-out laptop still displays
// the data. The assets, whose URLs carry a content hash, keep their
// year-long caching: that separation is why this is set by the renderer
// rather than by the middleware above.
func noStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Pragma", "no-cache")
}

// wantsFragment reports whether this is an htmx request asking for a
// piece of a page rather than a whole one.
//
// It exists so error rendering can answer in the shape the caller
// expects: swapping a full document with its <html> and <head> into a
// <div> produces a page that looks broken in a way nobody can diagnose
// from the screenshot.
func wantsFragment(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(r.Header.Get("HX-Request"), "true") {
		return false
	}
	// htmx sets HX-Boosted on a link or form it upgraded, and those
	// swap the whole body, so they want the full page after all.
	return !strings.EqualFold(r.Header.Get("HX-Boosted"), "true")
}
