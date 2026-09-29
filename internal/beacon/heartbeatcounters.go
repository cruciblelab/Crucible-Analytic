package beacon

import "github.com/cruciblelab/crucible-analytic/internal/heartbeat"

// writerCounts is the part of Writer that HeartbeatCounters reads, so a
// test can hand it fixed numbers.
type writerCounts interface {
	Counters() (written, dropped uint64)
}

// HeartbeatCounters is what the beacon reports in its heartbeat row:
// what the server and the writer have done, keyed as the panel reads
// them.
//
// A function here rather than the closure in main it used to be. A
// closure in main is out of every test's reach, and this project has
// paid for that once already (P5a: a live setting whose only wiring was
// in main, so no test could see that the wiring was missing). The
// refusals by reason are the reason it moved: a main that forgot to
// merge them would compile, run and report a total with no parts.
func HeartbeatCounters(s *Server, w writerCounts) map[string]int64 {
	accepted, serverDropped, rejected := s.Counters()
	written, writerDropped := w.Counters()
	out := map[string]int64{
		heartbeat.CounterAccepted: heartbeat.Count(accepted),
		heartbeat.CounterRejected: heartbeat.Count(rejected),
		heartbeat.CounterWritten:  heartbeat.Count(written),
		// One number, from two places that both shed. A request refused
		// at the door and a row thrown away at the writer are the same
		// loss to a customer's numbers, and splitting them on the page
		// would ask an operator to add up two figures to answer one
		// question.
		heartbeat.CounterDropped: heartbeat.Count(serverDropped + writerDropped),
		// Always, zero included: its presence is how the panel knows this
		// build honours a pause.
		heartbeat.CounterPaused: heartbeat.Count(s.Held()),
	}
	for key, n := range s.RejectionCounters() {
		out[key] = n
	}
	return out
}
