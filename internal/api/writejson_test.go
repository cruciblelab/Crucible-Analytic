package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// failingWriter is a ResponseWriter whose body writes fail with err.
type failingWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// captureDefault points slog.Default at a buffer for the length of the
// test. writeJSON logs through the default rather than a server's logger,
// and no test in this package runs in parallel.
func captureDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Both halves, because the first alone passes if the branch logged
// nothing at all: a write the deadline has already answered is quiet,
// and any other failed write still says so.
func TestWriteJSONIsQuietOnlyAboutTheDeadlinesOwnFailure(t *testing.T) {
	buf := captureDefault(t)
	writeJSON(failingWriter{httptest.NewRecorder(), http.ErrHandlerTimeout}, http.StatusOK, map[string]int{"n": 1})
	if buf.Len() != 0 {
		t.Errorf("a write after the deadline answered was logged: %s\n"+
			"internal/deadline already logged that request; this line called itself an "+
			"encoding failure beside it - measured, one per timed-out request", buf.String())
	}

	buf.Reset()
	writeJSON(failingWriter{httptest.NewRecorder(), errors.New("broken pipe")}, http.StatusOK, map[string]int{"n": 1})
	if !strings.Contains(buf.String(), "api: encoding response failed") {
		t.Errorf("a write that failed for another reason was not logged: %q", buf.String())
	}
}
