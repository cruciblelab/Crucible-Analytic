package proxy

import (
	"log/slog"
	"runtime/debug"
)

// recoverConn contains a panic to the one connection that caused it,
// instead of letting it take the process down.
//
// Go tears down the entire process on an unrecovered panic in any
// goroutine. This proxy sits in front of the customer's website, so
// "the process dies" means "the customer's site goes down" - and every
// other visitor's connection dies alongside the one that panicked. An
// attacker who found the input that triggers it only has to send it
// again after each restart, so a supervisor restarting the binary does
// not make it any less of an outage.
//
// The limiter's fail_open mode already commits this project to the rule
// that the collector must never be the reason a site is unreachable.
// This is that same rule applied to bugs rather than to load. net/http
// makes the same choice in conn.serve, which is why every other server
// in this repository already had this protection and only this package
// - the one that does not run on http.Server - lacked it.
//
// Deliberately loud: recovering a panic is not the same as it being
// fine. The stack is logged at Error precisely so that "one connection
// survived" never quietly becomes "nobody found out". Callers must defer
// this at the top of every goroutine they start, because recover() only
// sees panics raised on its own goroutine - a recover in the connection
// handler does nothing for the splice goroutines it spawns.
func recoverConn(logger *slog.Logger, where string) {
	r := recover()
	if r == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("proxy: recovered from panic, dropping this connection only",
		"where", where,
		"panic", r,
		"stack", string(debug.Stack()),
	)
}
