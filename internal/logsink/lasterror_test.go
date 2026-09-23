package logsink

import (
	"errors"
	"log/slog"
	"testing"
)

// unwritten is a sink with no writer goroutine and room for one record,
// so the second record is dropped - the full buffer, with no database.
func unwritten() *Sink {
	return &Sink{records: make(chan record, 1)}
}

func TestTheLastErrorIsTheNewestErrorLine(t *testing.T) {
	s := unwritten()
	logger := slog.New(s.Handler())

	if text, at := s.LastError(); text != "" || !at.IsZero() {
		t.Fatalf("a fresh sink reports %q at %v, want nothing", text, at)
	}

	logger.Error("api: query failed", "path", "/api/v1/sites", "err", errors.New("permission denied"))
	text, at := s.LastError()
	if text != "api: query failed: permission denied" {
		t.Errorf("last error = %q, want the message and its err", text)
	}
	if at.IsZero() {
		t.Error("the last error has no time")
	}

	// A WARN is not an error. The deadline's line is a WARN, and a slow
	// request is not a broken service.
	logger.Warn("request ran past its deadline and was answered 503")
	if got, _ := s.LastError(); got != "api: query failed: permission denied" {
		t.Errorf("a WARN replaced the last error: %q", got)
	}

	// This one is dropped - the buffer holds the first - and still names
	// the failure: a full buffer is a service in trouble.
	logger.With("err", "connection reset").Error("beacon: write failed, batch dropped")
	if got, _ := s.LastError(); got != "beacon: write failed, batch dropped: connection reset" {
		t.Errorf("a dropped ERROR, with its err given through With, left the last error at %q", got)
	}
	if _, dropped, _ := s.Counters(); dropped == 0 {
		t.Fatal("the fixture did not drop anything - the case above was not the dropped one")
	}

	// No err attribute: the message alone.
	logger.Error("upgrade: schema file refused")
	if got, _ := s.LastError(); got != "upgrade: schema file refused" {
		t.Errorf("last error = %q, want the bare message", got)
	}
}

// The text is the sanitized copy the table would hold: a log line is text
// somebody else chose, and this one is drawn on a page.
//
// Both halves carry a control character, because the text is built from
// two of them: a message cleaned and an err left raw would pass a test
// that dirtied only one.
func TestTheLastErrorIsSanitized(t *testing.T) {
	s := unwritten()
	slog.New(s.Handler()).Error("query\x01failed", "err", "bad\x00input\nsecond line")
	got, _ := s.LastError()
	if got == "" {
		t.Fatal("no last error was kept")
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Errorf("last error %q carries the control character %U", got, r)
		}
	}
}

// Lost is both ways a line fails to arrive, and only those.
func TestLostCountsDroppedAndFailedLines(t *testing.T) {
	s := unwritten()
	logger := slog.New(s.Handler())
	logger.Warn("one")   // buffered
	logger.Warn("two")   // dropped
	logger.Warn("three") // dropped
	s.failed.Add(4)
	s.written.Add(9)
	if got := s.Lost(); got != 6 {
		t.Errorf("Lost = %d, want 2 dropped + 4 failed = 6 (written lines are not lost)", got)
	}
}
