package devgate

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
)

const testPassword = "gelistirici-sifresi-123456"

// clock is a hand-wound clock, so expiry can be tested without sleeping
// through it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestGate(t *testing.T, opts Options) (*Gate, *clock) {
	t.Helper()
	hash, err := argon2id.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c := &clock{now: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.Now == nil {
		opts.Now = c.Now
	}
	gate, err := New(Config{PasswordHash: hash}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gate, c
}

func TestVerify_GrantsTheRightPasswordAndRefusesTheWrongOne(t *testing.T) {
	gate, _ := newTestGate(t, Options{})
	ctx := context.Background()

	granted := gate.Verify(ctx, Request{Actions: []string{"privacy.ip_storage"}, Password: testPassword})
	if !granted.OK() {
		t.Fatalf("the correct password was refused: %s", granted.Decision)
	}
	if !granted.For("privacy.ip_storage").Authorizes("privacy.ip_storage") {
		t.Error("a granted result did not authorize the action it was requested for")
	}

	refused := gate.Verify(ctx, Request{Actions: []string{"privacy.ip_storage"}, Password: "yanlis-sifre-1234"})
	if refused.OK() {
		t.Fatal("the wrong password was accepted")
	}
	if refused.Decision != DecisionWrongPassword {
		t.Errorf("decision = %q, want %q", refused.Decision, DecisionWrongPassword)
	}
	if refused.For("privacy.ip_storage").Authorizes("privacy.ip_storage") {
		t.Error("a refused result still authorized the action")
	}
}

// The property the whole design rests on: an authorization earned for
// one setting must not be spendable on another. Otherwise verifying to
// change the log retention would silently also permit turning IP
// masking off, and a caller could pass around the cheapest
// authorization it could get.
func TestAuthorization_IsBoundToItsAction(t *testing.T) {
	gate, _ := newTestGate(t, Options{})

	result := gate.Verify(context.Background(), Request{
		Actions:  []string{"logs.retention_days"},
		Password: testPassword,
	})
	if !result.OK() {
		t.Fatalf("verify: %s", result.Decision)
	}

	auth := result.For("logs.retention_days")
	if !auth.Authorizes("logs.retention_days") {
		t.Fatal("the authorization did not permit its own action")
	}
	if auth.Authorizes("privacy.ip_storage") {
		t.Error("an authorization for one setting permitted a different one")
	}
	if auth.Authorizes("") {
		t.Error("an authorization permitted the empty action")
	}
	// An action that was never requested must come back as the zero
	// value, not as something that happens to work.
	if result.For("campaign.extra_params").Authorizes("campaign.extra_params") {
		t.Error("an action that was never requested came back authorized")
	}
}

// "Her seferinde şifre sorulur" - asked every single time. A caller that
// stashes an authorization to save the user a second prompt must find it
// useless, or the rule is only an intention.
func TestAuthorization_ExpiresSoStashingItDoesNotWork(t *testing.T) {
	gate, c := newTestGate(t, Options{})

	result := gate.Verify(context.Background(), Request{
		Actions:  []string{"privacy.ip_storage"},
		Password: testPassword,
	})
	if !result.OK() {
		t.Fatalf("verify: %s", result.Decision)
	}
	stashed := result.For("privacy.ip_storage")
	if !stashed.Authorizes("privacy.ip_storage") {
		t.Fatal("a fresh authorization was not valid")
	}

	c.advance(GrantLifetime + time.Second)
	if stashed.Authorizes("privacy.ip_storage") {
		t.Error("an authorization kept past its lifetime still worked; it has become a session")
	}
}

// Any other package can declare an Authorization. None can make a valid
// one - the field that decides is unexported and has no setter. This
// test states the property; the compiler is what enforces it.
func TestAuthorization_ZeroValueAuthorizesNothing(t *testing.T) {
	var forged Authorization
	if forged.Authorizes("privacy.ip_storage") {
		t.Error("a zero Authorization authorized an action")
	}
	if forged.Authorizes("") {
		t.Error("a zero Authorization authorized the empty action")
	}
}

func TestNew_RefusesAPlaintextPasswordInTheConfig(t *testing.T) {
	_, err := New(Config{Password: "cok-gizli-sifre-1"}, Options{Logger: quietLogger()})
	if err == nil {
		t.Fatal("a plaintext password in the config was accepted; it would sit readable on disk")
	}
	if !strings.Contains(err.Error(), "password_hash") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

// A mistyped hash and a wrong password are indistinguishable at
// verification time, so the typo has to be caught while there is still
// something to point at.
func TestNew_RefusesAMalformedHash(t *testing.T) {
	for name, hash := range map[string]string{
		"not phc":         "gelistirici-sifresi-123456",
		"truncated":       "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
		"absurd cost":     "$argon2id$v=19$m=16777216,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"wrong algorithm": "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{PasswordHash: hash}, Options{Logger: quietLogger()}); err == nil {
				t.Error("a hash that can never verify was accepted at startup")
			}
		})
	}
}

// Fail closed. With no developer password there is nothing to check
// against, so the guarded settings stay at their defaults - which are
// the privacy-preserving values, so nothing worth having is lost.
func TestVerify_WithNoPasswordConfiguredTheGateIsShut(t *testing.T) {
	gate, err := New(Config{}, Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if gate.Configured() {
		t.Error("Configured() is true with no hash")
	}

	result := gate.Verify(context.Background(), Request{
		Actions:  []string{"privacy.ip_storage"},
		Password: "herhangi-bir-sifre",
	})
	if result.OK() {
		t.Fatal("an unconfigured gate granted an authorization")
	}
	if result.Decision != DecisionNotConfigured {
		t.Errorf("decision = %q, want %q", result.Decision, DecisionNotConfigured)
	}
	if !strings.Contains(Explain(result), "password_hash") {
		t.Errorf("the message does not say how to configure it: %q", Explain(result))
	}
}

func TestVerify_ThrottlesRepeatedFailures(t *testing.T) {
	gate, c := newTestGate(t, Options{})
	ctx := context.Background()

	for i := 0; i < maxFailures; i++ {
		result := gate.Verify(ctx, Request{Actions: []string{"privacy.ip_storage"}, Password: "yanlis-sifre-1234"})
		if result.Decision != DecisionWrongPassword {
			t.Fatalf("attempt %d: decision = %q, want %q", i+1, result.Decision, DecisionWrongPassword)
		}
	}

	blocked := gate.Verify(ctx, Request{Actions: []string{"privacy.ip_storage"}, Password: "yanlis-sifre-1234"})
	if blocked.Decision != DecisionThrottled {
		t.Fatalf("decision = %q, want %q", blocked.Decision, DecisionThrottled)
	}
	if blocked.RetryAfter <= 0 {
		t.Error("a throttled result gave no retry hint")
	}

	// Throttling must hold even against the correct password. Otherwise
	// the limit only slows down the attacker who is going to fail
	// anyway, and stops being a limit exactly when it matters.
	stillBlocked := gate.Verify(ctx, Request{Actions: []string{"privacy.ip_storage"}, Password: testPassword})
	if stillBlocked.OK() {
		t.Error("throttling was bypassed by the correct password")
	}

	// The window is a window, not a lockout.
	c.advance(throttleWindow + time.Second)
	recovered := gate.Verify(ctx, Request{Actions: []string{"privacy.ip_storage"}, Password: testPassword})
	if !recovered.OK() {
		t.Errorf("the gate stayed shut after the window passed: %s", recovered.Decision)
	}
}

func TestVerify_SuccessClearsEarlierFailures(t *testing.T) {
	gate, _ := newTestGate(t, Options{})
	ctx := context.Background()

	for i := 0; i < maxFailures-1; i++ {
		gate.Verify(ctx, Request{Actions: []string{"a"}, Password: "yanlis-sifre-1234"})
	}
	if result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: testPassword}); !result.OK() {
		t.Fatalf("verify: %s", result.Decision)
	}

	// Somebody who mistyped four times and then got it right should not
	// start their next change one strike from a lockout.
	for i := 0; i < maxFailures-1; i++ {
		result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: "yanlis-sifre-1234"})
		if result.Decision != DecisionWrongPassword {
			t.Fatalf("failures were not cleared by the successful attempt: %s", result.Decision)
		}
	}
}

// An empty field is a person who has not typed yet. It must not cost an
// argon2 computation, and it must not count towards a lockout, or a form
// bug that submits empty becomes a denial of service against the
// operator.
func TestVerify_EmptyPasswordIsFreeAndNotAFailure(t *testing.T) {
	gate, _ := newTestGate(t, Options{})
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < maxFailures*3; i++ {
		result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: ""})
		if result.Decision != DecisionNoPassword {
			t.Fatalf("decision = %q, want %q", result.Decision, DecisionNoPassword)
		}
	}
	elapsed := time.Since(start)

	// Fifteen real argon2 runs would be the better part of a second.
	if elapsed > 200*time.Millisecond {
		t.Errorf("%d empty submissions took %v - they are reaching argon2", maxFailures*3, elapsed)
	}
	if result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: testPassword}); !result.OK() {
		t.Errorf("empty submissions locked the gate: %s", result.Decision)
	}
}

func TestVerify_RefusesWithNoActionRequested(t *testing.T) {
	gate, _ := newTestGate(t, Options{})
	result := gate.Verify(context.Background(), Request{Password: testPassword})
	if result.OK() {
		t.Fatal("a verification with no action granted something")
	}
	if result.Decision != DecisionNoAction {
		t.Errorf("decision = %q, want %q", result.Decision, DecisionNoAction)
	}
}

// One password entry, one argon2 computation, one authorization per
// setting the form is changing. Each is still bound to its own action.
func TestVerify_OnePasswordEntryCanCoverSeveralActions(t *testing.T) {
	gate, _ := newTestGate(t, Options{})

	result := gate.Verify(context.Background(), Request{
		Actions:  []string{"privacy.ip_storage", "analytics.retention_days"},
		Password: testPassword,
	})
	if !result.OK() {
		t.Fatalf("verify: %s", result.Decision)
	}
	for _, action := range []string{"privacy.ip_storage", "analytics.retention_days"} {
		if !result.For(action).Authorizes(action) {
			t.Errorf("%s was not authorized", action)
		}
	}
	if result.For("privacy.ip_storage").Authorizes("analytics.retention_days") {
		t.Error("authorizations from one entry are interchangeable; they must not be")
	}
	if result.For("campaign.drop_params").Authorizes("campaign.drop_params") {
		t.Error("an action outside the request was authorized")
	}
}

// Every attempt is recorded, not only the refused ones. A record of
// failures alone cannot show that the successful entry came from an
// address that had been failing all hour.
func TestVerify_AuditsEveryAttempt(t *testing.T) {
	var mu sync.Mutex
	var seen []Attempt
	gate, _ := newTestGate(t, Options{
		Audit: func(_ context.Context, a Attempt) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, a)
			return nil
		},
	})
	ctx := context.Background()

	gate.Verify(ctx, Request{Actions: []string{"a"}, Password: "yanlis-sifre-1234", Actor: "ali", Peer: "10.0.0.9"})
	gate.Verify(ctx, Request{Actions: []string{"a"}, Password: testPassword, Actor: "ali", Peer: "10.0.0.9"})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("recorded %d attempts, want 2", len(seen))
	}
	if seen[0].OK() {
		t.Error("the failed attempt was recorded as successful")
	}
	if !seen[1].OK() {
		t.Error("the successful attempt was recorded as failed")
	}
	for i, a := range seen {
		if a.Actor != "ali" || a.Peer != "10.0.0.9" {
			t.Errorf("attempt %d lost its actor or peer: %+v", i, a)
		}
	}
}

// A failing audit sink must not decide the outcome. Refusing a correct
// password because the database blinked would be an outage; granting a
// wrong one because the record failed would be a hole.
func TestVerify_AFailingAuditHookChangesNothing(t *testing.T) {
	gate, _ := newTestGate(t, Options{
		Audit: func(context.Context, Attempt) error { return io.ErrUnexpectedEOF },
	})
	ctx := context.Background()

	if result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: testPassword}); !result.OK() {
		t.Errorf("a correct password was refused because auditing failed: %s", result.Decision)
	}
	if result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: "yanlis-sifre-1234"}); result.OK() {
		t.Error("a wrong password was granted because auditing failed")
	}
}

// Concurrent attempts must not multiply argon2's memory cost. With the
// queue bounded, the excess is refused rather than admitted, so a burst
// costs one verification's worth of memory instead of one per request.
func TestVerify_BoundsConcurrentWork(t *testing.T) {
	gate, _ := newTestGate(t, Options{})
	ctx := context.Background()

	const attempts = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[Decision]int{}

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			result := gate.Verify(ctx, Request{Actions: []string{"a"}, Password: "yanlis-sifre-1234"})
			mu.Lock()
			counts[result.Decision]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if counts[DecisionBusy]+counts[DecisionThrottled] == 0 {
		t.Errorf("%d concurrent attempts all reached argon2: %v", attempts, counts)
	}
	if counts[DecisionWrongPassword] > maxFailures+maxQueued+1 {
		t.Errorf("more attempts were hashed than the limits allow: %v", counts)
	}
}

// The password must never be read from a URL: a query string ends up in
// access logs, browser history and referrer headers.
func TestFromRequest_ReadsOnlyThePostBody(t *testing.T) {
	form := url.Values{FormField: {testPassword}}
	post := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := FromRequest(post); got != testPassword {
		t.Errorf("posted password = %q, want it read", got)
	}

	fromQuery := httptest.NewRequest(http.MethodPost, "/settings?"+form.Encode(), nil)
	if got := FromRequest(fromQuery); got != "" {
		t.Errorf("a password in the query string was honoured: %q", got)
	}

	get := httptest.NewRequest(http.MethodGet, "/settings?"+form.Encode(), nil)
	if got := FromRequest(get); got != "" {
		t.Errorf("a password on a GET was honoured: %q", got)
	}
}

func TestExplain_SaysSomethingUsefulForEveryDecision(t *testing.T) {
	for _, decision := range []Decision{
		DecisionWrongPassword, DecisionNoPassword, DecisionNotConfigured,
		DecisionThrottled, DecisionBusy, DecisionNoAction,
	} {
		message := Explain(Result{Decision: decision, RetryAfter: 90 * time.Second})
		if message == "" {
			t.Errorf("%s has no message", decision)
		}
	}
	if Explain(Result{Decision: DecisionGranted}) != "" {
		t.Error("a granted result produced a message to show the user")
	}
}
