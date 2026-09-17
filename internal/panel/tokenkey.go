package panel

import (
	"context"
	"fmt"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/tokenkey"
)

// Whether this deployment could honour privacy.ip_storage = "full".
//
// # What was wrong, and it was not subtle
//
// The gate asked a boolean field on the Store. The method that set it -
// SetIPTokenKeyConfigured - was called by two tests and by no production
// code, so on every real deployment the field was false and full mode
// could not be selected at all. The setup wizard's own check for the key
// read the same field and therefore reported "not configured" on every
// install, including the ones that had a key.
//
// A field nobody fills is not a weaker check. It is no check, wearing
// one's clothes - the same class as preflight.checkService, found in the
// same week, and the structural invariant written then did not catch
// this one because this is not a Config field: it is an argument.
//
// # Why the answer comes from the services
//
// The question is not "does a key exist somewhere". It is "can both
// writers of an address produce the same token", and the key lives in
// collector.toml and beacon.toml - files the panel's role cannot read,
// deliberately, because they carry database passwords for roles the
// panel must never hold.
//
// So each service answers for itself, in the one channel that already
// runs from a service to the panel: its heartbeat row. See
// internal/heartbeat/schema.sql for the column and what may never go
// in it.
//
// The reading of it lives in internal/tokenkey, because the setup
// wizard asks the same question and internal/panel/preflight may not
// import this package. What stays here is the refusal - an error is a
// panel concept, and ErrPreconditionUnmet is this package's sentinel.

// TokenKeyUnready is the refusal, carrying who said no.
//
// A typed error rather than a sentence, because the page has to name the
// services: "the collector has no key" and "the collector's build does
// not report one" have different fixes, and a reader told only that "the
// configuration is not ready" has to go and find out which of the two it
// was.
//
// Unwraps to ErrPreconditionUnmet, so every caller that already
// distinguishes a refusal from a fault keeps working without knowing
// this type exists.
type TokenKeyUnready struct {
	// Missing is every address writer that did not report a usable key.
	// Empty means no address writer has reported at all, which is a
	// different sentence: nothing is misconfigured, nothing has spoken.
	Missing []tokenkey.Report
}

func (e TokenKeyUnready) Error() string {
	if len(e.Missing) == 0 {
		return fmt.Sprintf("%v: no service that writes an address has reported an IP "+
			"token key yet", ErrPreconditionUnmet)
	}
	parts := make([]string, 0, len(e.Missing))
	for _, r := range e.Missing {
		switch r.State {
		case heartbeat.TokenKeyAbsent:
			parts = append(parts, r.Service+" reports no key")
		default:
			parts = append(parts, r.Service+" does not report one (build older than the column)")
		}
	}
	return fmt.Sprintf("%v: %s - put the same privacy.ip_hash_key in each service's "+
		"config file and restart it (generate one with: go run ./cmd/devpass -ipkey)",
		ErrPreconditionUnmet, strings.Join(parts, "; "))
}

// Unwrap lets errors.Is reach the sentinel.
func (e TokenKeyUnready) Unwrap() error { return ErrPreconditionUnmet }

// TokenKeyReports is what every address writer says about its key.
//
// Exported because the health page shows it: an operator who cannot
// switch to full mode should be able to see why on the page they were
// already looking at, rather than by trying the switch.
func (s *Store) TokenKeyReports(ctx context.Context) ([]tokenkey.Report, error) {
	return tokenkey.Reports(ctx, s.pool)
}

// tokenKeyReady reports whether full mode could be honoured, and the
// refusal when it could not.
func (s *Store) tokenKeyReady(ctx context.Context) error {
	reports, err := tokenkey.Reports(ctx, s.pool)
	if err != nil {
		// A question that could not be asked is not a yes. The
		// direction matters more than the tidiness: answering "ready"
		// on a failed query would let a switch into full mode land on a
		// deployment that writes no tokens, which is the silent
		// degradation this precondition exists for.
		return fmt.Errorf("%w: could not ask the services whether they hold an IP token "+
			"key: %v", ErrPreconditionUnmet, err)
	}
	if len(reports) == 0 {
		return TokenKeyUnready{}
	}
	if missing := tokenkey.Missing(reports); len(missing) > 0 {
		return TokenKeyUnready{Missing: missing}
	}
	return nil
}
