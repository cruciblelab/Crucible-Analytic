package deadline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log sink the handler goroutine and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func logTo(b *lockedBuffer) func() *slog.Logger {
	l := slog.New(slog.NewJSONHandler(b, nil))
	return func() *slog.Logger { return l }
}

var jsonAnswer = Answer{ContentType: "application/json", Body: `{"error":"too slow"}` + "\n"}

const limit = 150 * time.Millisecond

func TestAHandlerThatListensIsAnswered503AndLogged(t *testing.T) {
	logs := &lockedBuffer{}
	h := within(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), limit, jsonAnswer, logTo(logs))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sites/acme/summary", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want the answer's application/json", got)
	}
	if got := w.Body.String(); got != jsonAnswer.Body {
		t.Errorf("body = %q, want the answer %q", got, jsonAnswer.Body)
	}
	lines := logs.lines(t)
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want exactly 1: %v", len(lines), lines)
	}
	l := lines[0]
	if l["level"] != "WARN" {
		// WARN, not INFO: the panel's copy of a service's log keeps WARN
		// and above, and this line is for somebody with no shell.
		t.Errorf("level = %v, want WARN - the panel's log view starts there", l["level"])
	}
	if l["path"] != "/api/v1/sites/acme/summary" || l["method"] != "GET" {
		t.Errorf("line does not say which request: %v", l)
	}
	if l["deadline"] != "150ms" {
		t.Errorf("deadline = %v, want 150ms", l["deadline"])
	}
}

// The guarantee this package leans on TimeoutHandler for, held here so a
// future rewrite cannot quietly drop it: the answer arrives on time even
// from a handler that never looks at its context. Comparative, because
// "arrived in time" means nothing without the unwrapped run beside it.
func TestAHandlerThatIgnoresItsContextIsStillAnsweredOnTime(t *testing.T) {
	const sleep = 2 * time.Second
	deaf := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sleep)
		w.WriteHeader(http.StatusOK)
	})

	started := time.Now()
	deaf.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if unwrapped := time.Since(started); unwrapped < sleep {
		t.Fatalf("the unwrapped handler returned in %v - the fixture is not slow", unwrapped)
	}

	logs := &lockedBuffer{}
	w := httptest.NewRecorder()
	started = time.Now()
	within(deaf, limit, jsonAnswer, logTo(logs)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	took := time.Since(started)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if took >= sleep/2 {
		t.Errorf("answered after %v; the deadline is %v and the handler sleeps %v - "+
			"the answer waited for a handler that was not listening", took, limit, sleep)
	}
	if len(logs.lines(t)) != 1 {
		t.Errorf("want one log line for the deadline")
	}
}

func TestAFastHandlerIsPassedThroughUntouched(t *testing.T) {
	logs := &lockedBuffer{}
	h := within(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("X-Own", "yes")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "a,b\n")
	}), limit, jsonAnswer, logTo(logs))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusCreated || w.Body.String() != "a,b\n" {
		t.Errorf("got %d %q, want 201 %q", w.Code, w.Body.String(), "a,b\n")
	}
	if w.Header().Get("Content-Type") != "text/csv" || w.Header().Get("X-Own") != "yes" {
		t.Errorf("headers changed: %v", w.Header())
	}
	if n := len(logs.lines(t)); n != 0 {
		t.Errorf("%d log lines for a request that was on time, want 0", n)
	}
}

// The content type belongs to the 503 alone. A handler that relies on
// sniffing must not be told its HTML is JSON.
func TestAFastHandlerIsNotGivenTheAnswersContentType(t *testing.T) {
	h := within(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<!doctype html><p>hi</p>")
	}), limit, jsonAnswer, logTo(&lockedBuffer{}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := w.Header().Get("Content-Type"); got == "application/json" {
		t.Errorf("Content-Type = %q: the answer's type was put on an on-time response", got)
	}
}

// A handler's own 503 is not a deadline. Without this case the writer's
// check could be "any 503" and every test above would still pass.
func TestAHandlersOwn503IsNotReportedAsADeadline(t *testing.T) {
	logs := &lockedBuffer{}
	h := within(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}), limit, jsonAnswer, logTo(logs))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the handler's own 503", w.Code)
	}
	if n := len(logs.lines(t)); n != 0 {
		t.Errorf("%d log lines; a handler's own 503 was reported as a deadline", n)
	}
	if got := w.Header().Get("Content-Type"); got == "application/json" {
		t.Errorf("the answer's content type was put on a handler's own 503")
	}
}

// The status half of answerWriter's check, on its own. A handler that
// finishes in the instant the deadline passes is answered with its own
// response; that race cannot be staged reliably through TimeoutHandler,
// so the writer is asked directly: past the deadline, a 200 is not ours.
func TestAWriteAfterTheDeadlineThatIsNotA503IsNotCounted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	w := &answerWriter{ResponseWriter: httptest.NewRecorder(), ctx: ctx, contentType: "application/json"}
	w.WriteHeader(http.StatusOK)
	if w.timedOut {
		t.Error("a 200 written after the deadline was counted as the deadline's answer - " +
			"the log would say 503 for a response that was the handler's own")
	}
	if got := w.Header().Get("Content-Type"); got != "" {
		t.Errorf("the answer's content type %q was put on the handler's own response", got)
	}

	w = &answerWriter{ResponseWriter: httptest.NewRecorder(), ctx: ctx, contentType: "application/json"}
	w.WriteHeader(http.StatusServiceUnavailable)
	if !w.timedOut {
		t.Error("a 503 written after the deadline was not counted - the writer counts nothing")
	}
}

// The defect itself, over a real connection: a real http.Server with a
// WriteTimeout, a handler slower than it, and a client that reads what
// arrives. Without the wrapper the client gets nothing; with it, the
// answer. Both halves, because the second alone would pass on a server
// whose WriteTimeout had never truncated anything.
func TestOverARealConnectionTheAnswerArrivesWhereNothingDidBefore(t *testing.T) {
	const writeTimeout = time.Second
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * writeTimeout):
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})

	serve := func(t *testing.T, h http.Handler) (status string, body string, err error) {
		t.Helper()
		ln, lerr := net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			t.Fatal(lerr)
		}
		srv := &http.Server{Handler: h, WriteTimeout: writeTimeout}
		go srv.Serve(ln)
		defer srv.Close()
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			return "", "", err
		}
		defer resp.Body.Close()
		b, rerr := io.ReadAll(resp.Body)
		return resp.Status, string(b), rerr
	}

	status, body, err := serve(t, slow)
	if err == nil {
		t.Fatalf("without the wrapper the client got %q %q - the fixture does not "+
			"reproduce the truncation this package exists for", status, body)
	}
	logs := &lockedBuffer{}
	status, body, err = serve(t, within(slow, 300*time.Millisecond, jsonAnswer, logTo(logs)))
	if err != nil {
		t.Fatalf("with the wrapper the client still got an error: %v", err)
	}
	if !strings.HasPrefix(status, "503") || body != jsonAnswer.Body {
		t.Errorf("got %q %q, want 503 %q", status, body, jsonAnswer.Body)
	}
	if len(logs.lines(t)) != 1 {
		t.Errorf("want one log line")
	}
}

// For, with the expected values spelled out rather than computed from
// Margin: a test that took them from the constant would move with it.
func TestForKeepsFiveSecondsBack(t *testing.T) {
	for _, c := range []struct{ wt, want time.Duration }{
		{60 * time.Second, 55 * time.Second},
		{15 * time.Second, 10 * time.Second},
		{10*time.Second + time.Millisecond, 5*time.Second + time.Millisecond},
	} {
		if got := For(c.wt); got != c.want {
			t.Errorf("For(%v) = %v, want %v", c.wt, got, c.want)
		}
	}
}

// Both sides of the refusal: 10 s is the first WriteTimeout with no room.
func TestForRefusesAWriteTimeoutWithNoRoom(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("For(10s) returned; a deadline of 5 s beside a margin of 5 s " +
				"leaves the handler as much time as the answer")
		}
	}()
	For(10 * time.Second)
}

// LogPath names the request in the line; nil leaves the path as it came.
// The nil half is TestAHandlerThatListensIsAnswered503AndLogged above.
func TestTheLineNamesTheRequestThroughLogPath(t *testing.T) {
	logs := &lockedBuffer{}
	answer := jsonAnswer
	answer.LogPath = func(p string) string { return "/katil/[redacted]" }
	h := within(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), limit, answer, logTo(logs))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/katil/SECRET", nil))

	lines := logs.lines(t)
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want 1", len(lines))
	}
	if lines[0]["path"] != "/katil/[redacted]" {
		t.Errorf("path = %v, want what LogPath returned", lines[0]["path"])
	}
}
