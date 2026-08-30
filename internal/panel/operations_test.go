package panel

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The error chain is the part of an operation record somebody actually
// reads when they are trying to fix something, and it is stored whole
// rather than as its last link. This file is why that claim is a fact
// and not a comment.
//
// It exists because a mutation survived: replacing the whole loop in
// errorChain with `return err.Error()` left every test green. The
// integration test that looked like it covered this asserted the chain
// contained "3650", and the outermost link already carried it - so the
// assertion passed on a chain of one, and the design decision the
// comment describes was unguarded.

// TestTheChainKeepsTheCauseAndNotOnlyTheSentence.
//
// Built with the store's own wrapper rather than a hand-made error, so
// the test moves when production wrapping moves. The two links say
// different things and only one of them is actionable:
//
//	outer  panel: store setting logs.level:        what the panel was doing
//	inner  permission denied for table ... 42501:  what to actually fix
//
// A record that kept only the outer sentence would tell an operator
// that a setting failed to save, which they already knew.
func TestTheChainKeepsTheCauseAndNotOnlyTheSentence(t *testing.T) {
	cause := errors.New(`ERROR: permission denied for table panel_settings (SQLSTATE 42501)`)
	wrapped := wrapStoreError(KeyLogLevel, cause)

	chain := errorChain(wrapped)

	if !strings.Contains(chain, "store setting") {
		t.Errorf("the chain lost the outer sentence, which names what was being done:\n%s", chain)
	}
	if !strings.Contains(chain, "42501") {
		t.Errorf("the chain lost the cause, which is the only link that names the fix:\n%s", chain)
	}
	// Two links means two lines. One line means somebody replaced the
	// walk with err.Error(), which passes both assertions above by
	// accident: fmt.Errorf's %w leaves the cause's text inside the outer
	// error's own message.
	if lines := strings.Count(chain, "\n") + 1; lines != 2 {
		t.Errorf("the chain has %d line(s), want 2 — it is being stored as its outermost link, not as a chain:\n%s",
			lines, chain)
	}
}

// TestTheChainDoesNotRepeatALinkThatAddedNothing.
//
// A wrapper that adds no text of its own - errors.Join of one, some
// custom types - unwraps to something whose message equals its own.
// Printing both would double every line for no information.
func TestTheChainDoesNotRepeatALinkThatAddedNothing(t *testing.T) {
	inner := errors.New("bir sebep")
	chain := errorChain(sameTextWrapper{inner})
	if got := strings.Count(chain, "bir sebep"); got != 1 {
		t.Errorf("the cause appears %d times in %q; a link that added no text was printed anyway", got, chain)
	}
}

type sameTextWrapper struct{ err error }

func (w sameTextWrapper) Error() string { return w.err.Error() }
func (w sameTextWrapper) Unwrap() error { return w.err }

// TestAChainThatNeverEndsStillReturns.
//
// The depth cap. An error whose Unwrap returns something equally deep
// forever is not hypothetical - it is what a wrapper with a cycle in it
// does - and this runs on the path that closes a failed operation, which
// is the worst place to hang.
func TestAChainThatNeverEndsStillReturns(t *testing.T) {
	// 40 links, past the cap of 16.
	var err error = errors.New("dip")
	for i := 0; i < 40; i++ {
		err = fmt.Errorf("katman %d: %w", i, err)
	}

	done := make(chan string, 1)
	go func() { done <- errorChain(err) }()

	select {
	case chain := <-done:
		if lines := strings.Count(chain, "\n") + 1; lines > 16 {
			t.Errorf("the chain walked %d links; the cap is 16", lines)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("errorChain did not return")
	}
}

// TestNoErrorIsAnEmptyChain.
//
// Finish is called on success too, and a success that recorded the text
// of some unrelated error would be worse than one recording nothing.
func TestNoErrorIsAnEmptyChain(t *testing.T) {
	if chain := errorChain(nil); chain != "" {
		t.Errorf("errorChain(nil) = %q, want empty", chain)
	}
}
