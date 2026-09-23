// Package deadline gives a server's handlers a deadline shorter than the
// server's own WriteTimeout, so a request that runs too long is answered -
// with a 503 that says so - instead of being dropped without a byte.
//
// # The defect, measured
//
// An http.Server's WriteTimeout does not stop a handler. It sets a
// deadline on the connection, the handler keeps running, and whatever it
// writes afterwards goes into a buffer that is never delivered. Two
// services, both measured on their real binaries:
//
//	read API, pool of one connection, eight concurrent 90-day requests:
//	  four answered in 23-55 s; four got 0 bytes and a closed
//	  connection at 65-98 s - no status line - and the log had no line.
//	panel, GET / while panel_users is locked for 75 s (the lock a schema
//	  upgrade takes): 0 bytes at 74.0 s, and the access log wrote
//	  status=200 ms=74046 for a response nobody received.
//
// The API's writeJSON had an error branch for exactly this, and it never
// ran: the body sat in the response buffer and the failing write was the
// flush after the handler returned, which the handler never sees. The
// panel's access log records what the handler wrote, not what the
// connection carried.
//
// # What this does
//
// The hard half is net/http's own TimeoutHandler, which answers when the
// deadline passes whether or not the handler is listening to its
// context - that guarantee is the point, and it is not rewritten here.
// This package adds what TimeoutHandler leaves out: the deadline is
// derived from the WriteTimeout it has to beat, the answer carries a
// content type only on the path that needs one, and the operator gets a
// line.
package deadline

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Margin is the part of a server's WriteTimeout kept back for writing
// the answer.
//
// A fixed amount rather than a fraction, because what it pays for does
// not scale with the timeout: it is the time to put one buffered
// response on the wire. Five seconds carries a 50 KB page to a client on
// a 100 kbit/s link, and the 503 itself is a few hundred bytes.
const Margin = 5 * time.Second

// For is the handler deadline for a server whose WriteTimeout is
// writeTimeout.
//
// It panics when there is no room for a margin: a server whose
// WriteTimeout is that short has no deadline this package can honestly
// give it, and a wrong wiring should fail the first test that builds the
// handler rather than run with a deadline of zero.
func For(writeTimeout time.Duration) time.Duration {
	if writeTimeout <= 2*Margin {
		panic("deadline: WriteTimeout " + writeTimeout.String() +
			" leaves no room for the " + Margin.String() + " margin")
	}
	return writeTimeout - Margin
}

// Answer is what a request that ran out of time is told.
type Answer struct {
	// ContentType is set on the 503 only. Setting it on every response
	// would overwrite nothing a handler set - TimeoutHandler copies the
	// handler's headers over - but it would stick to every response
	// whose handler relied on content sniffing.
	ContentType string
	// Body is the whole 503 body, written as given.
	Body string
	// LogPath is how the request's path appears in the log line; nil
	// writes it as it arrived. The panel sets it, because some of its
	// paths are credentials - measured, this line put an invitation
	// token into panel_logs before it did.
	LogPath func(string) string
	// OnTimeout is called once for each request this answered, beside
	// its log line; nil calls nothing. It is how the count reaches the
	// health page: a line per request says what happened, a number says
	// how often, and only the number fits on a page read at a glance.
	OnTimeout func()
}

// Handler wraps h so it is answered by the deadline For(writeTimeout)
// gives, and logs when that happens.
//
// logger is a function rather than a *slog.Logger because the servers
// here resolve their logger on each call (a nil field means
// slog.Default), and the wrapper is built before main swaps the default.
func Handler(h http.Handler, writeTimeout time.Duration, answer Answer, logger func() *slog.Logger) http.Handler {
	return within(h, For(writeTimeout), answer, logger)
}

// within is Handler with the deadline given directly, so the tests can
// use one measured in milliseconds.
func within(h http.Handler, limit time.Duration, answer Answer, logger func() *slog.Logger) http.Handler {
	inner := http.TimeoutHandler(h, limit, answer.Body)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		// The same deadline TimeoutHandler sets for itself, made here so
		// this side can tell afterwards whether it was reached - and
		// handed to TimeoutHandler as the parent of its own, which is not
		// a nicety. ctx.Err() is set by a timer's callback, not computed
		// from the clock, and two timers set microseconds apart fire in
		// either order: with two unrelated contexts, the 503 could be
		// written while this one still read nil, and the request would be
		// neither logged nor typed. As a child, TimeoutHandler's context
		// is cancelled through this one, so this one is always done
		// first. A mutation passing r instead of r.WithContext(ctx) turned
		// two tests red.
		ctx, cancel := context.WithTimeout(r.Context(), limit)
		defer cancel()
		tw := &answerWriter{ResponseWriter: w, ctx: ctx, contentType: answer.ContentType}
		inner.ServeHTTP(tw, r.WithContext(ctx))
		if tw.timedOut {
			if answer.OnTimeout != nil {
				answer.OnTimeout()
			}
			path := r.URL.Path
			if answer.LogPath != nil {
				path = answer.LogPath(path)
			}
			logger().Warn("request ran past its deadline and was answered 503",
				"method", r.Method,
				"path", path,
				"deadline", limit.String(),
				"elapsed_ms", time.Since(started).Milliseconds(),
			)
		}
	})
}

// answerWriter sees the status TimeoutHandler writes and recognises its
// own 503: one written after this request's deadline passed.
//
// Recognised by what was written rather than by the context alone. A
// handler that finishes in the same instant the deadline passes is
// answered with its own response, and a line saying "answered 503" would
// then be false. One case still reads wrong, and it is left: a handler
// whose own answer is a 503, written in that same instant, is logged as
// a deadline - the status the client got is still the one the line
// names.
type answerWriter struct {
	http.ResponseWriter
	ctx         context.Context
	contentType string
	timedOut    bool
}

func (w *answerWriter) WriteHeader(code int) {
	if code == http.StatusServiceUnavailable && w.ctx.Err() == context.DeadlineExceeded {
		w.timedOut = true
		// On this path TimeoutHandler has copied none of the handler's
		// headers, so there is no type of the handler's to preserve.
		if w.contentType != "" {
			w.Header().Set("Content-Type", w.contentType)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}
