package heartbeat

import (
	"strings"
	"testing"
	"time"
)

// fixedLog is a LogReport with a fixed answer.
type fixedLog struct {
	lost uint64
	text string
	at   time.Time
}

func (f fixedLog) Lost() uint64                   { return f.lost }
func (f fixedLog) LastError() (string, time.Time) { return f.text, f.at }

func TestTheRowCarriesTheLogCopysLossAndLastError(t *testing.T) {
	at := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
	own := map[string]int64{CounterWritten: 10}
	r := New(Options{
		Counters: func() map[string]int64 { return own },
		Log:      fixedLog{lost: 4, text: "api: query failed: permission denied", at: at},
	})

	text, gotAt, counters := r.snapshot()
	if text != "api: query failed: permission denied" || !gotAt.Equal(at) {
		t.Errorf("last error = %q at %v, want the log copy's", text, gotAt)
	}
	if counters[CounterLogLost] != 4 {
		t.Errorf("counters = %v, want %s = 4", counters, CounterLogLost)
	}
	if counters[CounterWritten] != 10 {
		t.Errorf("counters = %v, lost the service's own", counters)
	}
	// The service's map is the service's: a reporter that wrote into it
	// would put its own key into a number the service owns, on every
	// beat, for as long as the process lives.
	if _, ok := own[CounterLogLost]; ok {
		t.Errorf("the reporter wrote into the service's own map: %v", own)
	}
}

// A service with no log copy reports no loss count at all, rather than a
// zero: "nothing lost" and "nothing to lose" are different sentences, and
// the page draws only the keys a row carries.
func TestNoLogCopyMeansNoLossCounter(t *testing.T) {
	r := New(Options{Counters: func() map[string]int64 { return map[string]int64{CounterWritten: 1} }})
	text, at, counters := r.snapshot()
	if _, ok := counters[CounterLogLost]; ok {
		t.Errorf("counters = %v, want no %s without a log copy", counters, CounterLogLost)
	}
	if text != "" || !at.IsZero() {
		t.Errorf("last error = %q at %v, want none", text, at)
	}
}

func TestALongErrorIsCutForTheRow(t *testing.T) {
	long := strings.Repeat("ş", 600)
	r := New(Options{Log: fixedLog{text: long, at: time.Now()}})
	text, _, _ := r.snapshot()
	if n := len([]rune(text)); n != 501 {
		t.Errorf("last error is %d runes, want 500 and the ellipsis", n)
	}
}
