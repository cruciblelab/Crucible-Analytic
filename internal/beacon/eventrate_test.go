package beacon

import (
	"fmt"
	"net/http"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

func fromAddr(addr string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = netip.AddrPortFrom(netip.MustParseAddr(addr), 41234).String() }
}

// sendBurst posts n page views at the test server's one instant and
// returns the status of each.
func sendBurst(t *testing.T, s *Server, n int, mutate ...func(*http.Request)) []int {
	t.Helper()
	out := make([]int, n)
	for i := range out {
		out[i] = post(t, s, pageview, mutate...).Code
	}
	return out
}

// Within one burst window, at the server's one instant: the events up to the
// mark are people, the ones past it are written as 'rate', and the ones
// past the drop are not written at all and are counted as refused. Each
// threshold is crossed by exactly one event, so a mark read as ">=" or a
// drop judged after the mark would move a row.
func TestTheEventRateMarksAndThenDrops(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.EventRate = EventRateLimits{MarkBurst: 3, DropBurst: 6}

	codes := sendBurst(t, s, 8)
	want := []int{204, 204, 204, 204, 204, 204, 429, 429}
	if fmt.Sprint(codes) != fmt.Sprint(want) {
		t.Errorf("statuses %v, want %v", codes, want)
	}
	rows := sink.all()
	if len(rows) != 6 {
		t.Fatalf("%d rows written, want 6 - the two over the drop must not reach the sink", len(rows))
	}
	for i, r := range rows {
		wantReason := ""
		if i >= 3 {
			wantReason = AutoReasonRate
		}
		if r.AutoReason != wantReason {
			t.Errorf("event %d: auto_reason %q, want %q", i+1, r.AutoReason, wantReason)
		}
	}
	if got := s.RejectionCounters()[heartbeat.CounterRejectedRate]; got != 2 {
		t.Errorf("reddedilen_hiz = %d, want 2", got)
	}
	// Another address is its own count: the flood is this one's.
	if w := post(t, s, pageview, fromAddr("198.51.100.7")); w.Code != 204 {
		t.Errorf("another address got %d", w.Code)
	}
	if r := sink.all()[6]; r.AutoReason != "" {
		t.Errorf("another address's first event marked %q", r.AutoReason)
	}
}

// The minute window on its own: events spread one a second never trip the
// burst limit, and the minute's still catches the 5th - and lets the
// address back in once its minute has passed.
func TestTheMinuteWindowHoldsAPacedFlood(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	clock := verdictNow.Truncate(time.Minute)
	s.Now = func() time.Time { return clock }
	s.EventRate = EventRateLimits{MarkBurst: 3, MarkPerMinute: 4}

	for i := 1; i <= 6; i++ {
		post(t, s, pageview)
		clock = clock.Add(time.Second)
	}
	var marked int
	for _, r := range sink.all() {
		if r.AutoReason == AutoReasonRate {
			marked++
		}
	}
	if marked != 2 {
		t.Errorf("%d of 6 paced events marked, want 2 (the 5th and 6th of the minute)", marked)
	}
	clock = clock.Add(2 * time.Minute)
	post(t, s, pageview)
	if r := sink.all()[6]; r.AutoReason != "" {
		t.Errorf("after two quiet minutes the first event is %q - the window never emptied", r.AutoReason)
	}
}

// The collector's reason is written over the rate's: the column holds one,
// and a signed verdict says more about the visitor than how fast its
// address is.
func TestTheCollectorsReasonComesBeforeTheRates(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.EventRate = EventRateLimits{MarkBurst: 1}
	post(t, s, pageview)
	post(t, s, pageview, withVerdict(signedFor(s, scoring.BotCutoff, verdictNow, verdictPeer)))
	post(t, s, pageview)
	got := []string{}
	for _, r := range sink.all() {
		got = append(got, r.AutoReason)
	}
	if want := []string{"", AutoReasonCollector, AutoReasonRate}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reasons %q, want %q", got, want)
	}
}

// A paused site's events are answered and not recorded, and they are not
// a rate either: after the pause the address starts from nothing.
func TestAPausedSitesEventsAreNotARate(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.EventRate = EventRateLimits{MarkBurst: 2, DropBurst: 4}
	s.SetPaused(map[string]time.Time{"acme": verdictNow.Add(time.Hour)})
	sendBurst(t, s, 10)
	s.SetPaused(nil)
	post(t, s, pageview)
	if r := sink.only(t); r.AutoReason != "" {
		t.Errorf("the first event after a paused burst is %q", r.AutoReason)
	}
	if got := s.RejectionCounters()[heartbeat.CounterRejectedRate]; got != 0 {
		t.Errorf("reddedilen_hiz = %d after a paused burst", got)
	}
}

// No limits, no table: a Server built without EventRate - every test's -
// judges nothing and keeps nothing in memory.
func TestNoLimitsKeepNoTable(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	sendBurst(t, s, 50)
	for _, r := range sink.all() {
		if r.AutoReason != "" {
			t.Fatalf("marked %q with no limits", r.AutoReason)
		}
	}
	if s.rates.addrs != nil {
		t.Errorf("the table holds %d addresses with no limits", len(s.rates.addrs))
	}
}

// One IPv4 address is one key; an IPv6 /64 is one key, whatever the
// interface half says; IPv4 written inside IPv6 is the IPv4 address.
func TestTheRateIsKeptPerAddressAndPerIPv6Slash64(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"192.0.2.1", "192.0.2.2", false},
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff", true},
		{"2001:db8:1:2::1", "2001:db8:1:3::1", false},
		{"::ffff:192.0.2.1", "192.0.2.1", true},
	} {
		ka, kb := rateKey(netip.MustParseAddr(c.a)), rateKey(netip.MustParseAddr(c.b))
		if (ka == kb) != c.same {
			t.Errorf("%s and %s: keys %v and %v, same = %v, want %v", c.a, c.b, ka, kb, ka == kb, c.same)
		}
	}
	// And through the handler: a flood rotating through its /64 is one
	// address's flood.
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.EventRate = EventRateLimits{MarkBurst: 2}
	for i := 1; i <= 3; i++ {
		post(t, s, pageview, fromAddr(fmt.Sprintf("2001:db8:1:2::%x", i)))
	}
	if r := sink.all()[2]; r.AutoReason != AutoReasonRate {
		t.Errorf("the third event from one /64 is %q, want %q", r.AutoReason, AutoReasonRate)
	}
}

// Each limit is a strict "more than": exactly the limit is still a person.
// Zero is off. The drop is judged before the mark.
func TestTheLimitsAreJudgedAsWritten(t *testing.T) {
	l := EventRateLimits{MarkBurst: 10, MarkPerMinute: 100, DropBurst: 20, DropPerMinute: 200}
	for _, c := range []struct {
		sec, min float64
		want     rateAction
	}{
		{10, 100, rateFine},
		{11, 0, rateMark},
		{0, 101, rateMark},
		{20, 0, rateMark},
		{21, 0, rateDrop},
		{0, 201, rateDrop},
		{21, 101, rateDrop},
	} {
		if got := l.judge(c.sec, c.min); got != c.want {
			t.Errorf("judge(%v, %v) = %v, want %v", c.sec, c.min, got, c.want)
		}
	}
	if got := (EventRateLimits{}).judge(1e9, 1e9); got != rateFine {
		t.Errorf("all limits off judged %v", got)
	}
}

// A full table judges no new address, says so in the heartbeat, and keeps
// judging the addresses it already holds. The event is written either way.
func TestAFullTableStopsJudgingNewAddressesOnly(t *testing.T) {
	var e eventRates
	l := EventRateLimits{MarkBurst: 1}
	for i := 0; i < maxRateAddresses; i++ {
		e.record(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), verdictNow, l)
	}
	// Twice: one event is under any mark, so only the second can show the
	// new address was judged after all.
	newcomer := netip.MustParseAddr("192.0.2.200")
	for i := 1; i <= 2; i++ {
		if act, _ := e.record(newcomer, verdictNow, l); act != rateFine {
			t.Errorf("event %d of a new address at a full table: %v, want it left unjudged", i, act)
		}
	}
	if n := e.untracked.Load(); n != 2 {
		t.Errorf("untracked = %d, want 2", n)
	}
	if n := len(e.addrs); n != maxRateAddresses {
		t.Errorf("the full table grew to %d", n)
	}
	held := netip.AddrFrom4([4]byte{10, 0, 0, 5})
	if act, _ := e.record(held, verdictNow, l); act != rateMark {
		t.Errorf("an address already in the full table: %v, want its second event marked", act)
	}
	// And the sweep empties it once the windows have.
	e.record(netip.MustParseAddr("192.0.2.201"), verdictNow.Add(2*time.Minute), l)
	if n := len(e.addrs); n != 1 {
		t.Errorf("after two quiet minutes the table holds %d addresses, want 1 (the one just recorded)", n)
	}
}

// The sweep runs on its schedule and forgets only what has emptied.
func TestTheSweepForgetsOnlyEmptiedAddresses(t *testing.T) {
	var e eventRates
	l := EventRateLimits{MarkBurst: 100}
	old, recent := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	e.record(old, verdictNow, l)
	e.record(recent, verdictNow.Add(90*time.Second), l)
	e.record(recent, verdictNow.Add(2*time.Minute+rateSweepEvery), l)
	if _, ok := e.addrs[rateKey(old)]; ok {
		t.Error("an address quiet for two minutes was kept")
	}
	if _, ok := e.addrs[rateKey(recent)]; !ok {
		t.Error("an address seen within the minute was forgotten")
	}

	// Kept means its count is kept, not only its key: a sweep that
	// forgot everything and let the next event re-add it would pass the
	// check above. Three events in one minute, a sweep before the second:
	// the third is over a mark of two only if the first was remembered.
	var f eventRates
	m := EventRateLimits{MarkPerMinute: 2}
	start := verdictNow.Truncate(time.Minute)
	f.record(recent, start, m)
	f.record(recent, start.Add(rateSweepEvery+time.Second), m)
	if act, _ := f.record(recent, start.Add(rateSweepEvery+2*time.Second), m); act != rateMark {
		t.Errorf("the third event of a minute, across a sweep, is %v - the sweep forgot a live address", act)
	}
}

// The log says when an address starts being dropped, once, and again only
// after it stopped.
func TestADropRunIsReportedOncePerRun(t *testing.T) {
	var e eventRates
	l := EventRateLimits{DropBurst: 1}
	ip := netip.MustParseAddr("192.0.2.9")
	var starts []bool
	for _, at := range []time.Duration{0, 0, 0, 3 * time.Second, 3 * time.Second} {
		_, started := e.record(ip, verdictNow.Add(at), l)
		starts = append(starts, started)
	}
	if want := []bool{false, true, false, false, true}; fmt.Sprint(starts) != fmt.Sprint(want) {
		t.Errorf("drop starts %v, want %v", starts, want)
	}
}

// The ceiling on the table is a memory claim, so it is measured: the table
// full of IPv6 /64s - the keys an attacker can mint for free - stays under
// 10 MB.
func TestTheRateTableStaysInItsBudget(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var e eventRates
	l := EventRateLimits{MarkBurst: 100}
	for i := 0; i < maxRateAddresses; i++ {
		a := [16]byte{0x20, 0x01, 0x0d, 0xb8, byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}
		e.record(netip.AddrFrom16(a), verdictNow, l)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d addresses: %d bytes, %d an address", len(e.addrs), used, used/int64(len(e.addrs)))
	if len(e.addrs) != maxRateAddresses {
		t.Fatalf("the table holds %d, want %d", len(e.addrs), maxRateAddresses)
	}
	if used > 10<<20 {
		t.Errorf("the full table takes %d bytes, over 10 MB", used)
	}
	runtime.KeepAlive(&e)
}

// The burst window is 100 ms and not a second: ten events 120 ms apart are
// nine in any second and never more than two in any 100 ms, so a mark of
// two leaves them alone; three inside 80 ms mark the third.
func TestTheBurstWindowIsATenthOfASecond(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	clock := verdictNow.Truncate(time.Second)
	s.Now = func() time.Time { return clock }
	s.EventRate = EventRateLimits{MarkBurst: 2}
	for i := 0; i < 10; i++ {
		post(t, s, pageview)
		clock = clock.Add(120 * time.Millisecond)
	}
	for i, r := range sink.all() {
		if r.AutoReason != "" {
			t.Fatalf("event %d of ten 120 ms apart marked %q - the window is wider than 100 ms", i+1, r.AutoReason)
		}
	}
	clock = clock.Add(time.Second)
	for i := 0; i < 3; i++ {
		post(t, s, pageview)
		clock = clock.Add(40 * time.Millisecond)
	}
	if r := sink.all()[12]; r.AutoReason != AutoReasonRate {
		t.Errorf("the third of three events inside 80 ms is %q, want %q", r.AutoReason, AutoReasonRate)
	}
}
