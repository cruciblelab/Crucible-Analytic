package mail

import (
	"errors"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

// The probe tests. Each one puts a server in a specific real state and
// asks what the diagnosis is, because the diagnosis is what this package
// is for: the panel already had a way to say "sending failed".

func TestProbeSTARTTLS(t *testing.T) {
	s := startServer(t, serverConfig{
		offerSTARTTLS:  true,
		authMechanisms: "PLAIN LOGIN",
		acceptUser:     "panel", acceptPass: "gizli",
	})

	p := s.config("panel", "gizli").Probe()
	if p.Err != nil {
		t.Fatalf("probe failed: %v", p.Err)
	}
	if !p.Reached || !p.TLSOffered || p.TLS != EncryptionSTARTTLS {
		t.Errorf("reached=%v offered=%v tls=%q, want a STARTTLS connection", p.Reached, p.TLSOffered, p.TLS)
	}
	if !p.Authenticated || p.Anonymous {
		t.Errorf("authenticated=%v anonymous=%v, want authenticated", p.Authenticated, p.Anonymous)
	}
	if p.Stage != StageDone {
		t.Errorf("stage = %q, want %q", p.Stage, StageDone)
	}
	if got := p.Diagnose(); got != DiagOK {
		t.Errorf("diagnosis = %q, want DiagOK", got)
	}
	if got := len(s.authAttempts()); got != 1 {
		t.Errorf("server saw %d AUTH commands, want 1", got)
	}
}

func TestProbeImplicitTLS(t *testing.T) {
	s := startServer(t, serverConfig{
		implicitTLS:    true,
		authMechanisms: "PLAIN",
		acceptUser:     "panel", acceptPass: "gizli",
	})

	p := s.config("panel", "gizli").Probe()
	if p.Err != nil {
		t.Fatalf("probe failed: %v", p.Err)
	}
	if p.TLS != EncryptionImplicit {
		t.Errorf("tls = %q, want %q", p.TLS, EncryptionImplicit)
	}
	// The server never advertises STARTTLS on an already-encrypted
	// connection, so TLSOffered is false here and Encrypted is the field
	// that tells the truth. Asserted rather than assumed, because a panel
	// showing "encryption: no" on a working 465 account would be a bug
	// nobody would think to look for.
	if p.TLSOffered {
		t.Error("TLSOffered is true on an implicit-TLS connection")
	}
	if !p.Encrypted() {
		t.Error("Encrypted() is false on an implicit-TLS connection")
	}
	if got := p.Diagnose(); got != DiagOK {
		t.Errorf("diagnosis = %q, want DiagOK", got)
	}
}

// TestProbeNeverSendsPasswordUnencrypted is the load-bearing test of this
// package, and it comes with its own control.
//
// The server is on 127.0.0.1, which is exactly the case where net/smtp's
// PlainAuth waives its own encryption requirement (auth.go, isLocalhost).
// So "no AUTH command arrived" here is not something the standard library
// would have given us for free - the control sub-test proves that by
// authenticating against the same server with plain net/smtp and watching
// the password go out.
//
// Without the control, a green test would be consistent with this package
// doing nothing at all, on a machine where the library happened to refuse.
func TestProbeNeverSendsPasswordUnencrypted(t *testing.T) {
	t.Run("this package refuses", func(t *testing.T) {
		s := startServer(t, serverConfig{
			offerSTARTTLS:  false,
			authMechanisms: "PLAIN",
			acceptUser:     "panel", acceptPass: "gizli",
		})

		p := s.config("panel", "gizli").Probe()
		if !errors.Is(p.Err, ErrWouldSendPasswordInClear) {
			t.Fatalf("err = %v, want ErrWouldSendPasswordInClear", p.Err)
		}
		if p.Authenticated {
			t.Error("reported as authenticated over an unencrypted connection")
		}
		if got := p.Diagnose(); got != DiagNoTLS {
			t.Errorf("diagnosis = %q, want DiagNoTLS", got)
		}
		if got := s.authAttempts(); len(got) != 0 {
			t.Errorf("the server received %d AUTH commands, want 0: %q", len(got), got)
		}
	})

	t.Run("net/smtp alone would not", func(t *testing.T) {
		s := startServer(t, serverConfig{
			offerSTARTTLS:  false,
			authMechanisms: "PLAIN",
			acceptUser:     "panel", acceptPass: "gizli",
		})

		host, _ := s.addr()
		conn, err := net.Dial("tcp", s.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()

		// No STARTTLS, no encryption, and the library sends the password
		// anyway because the host string is 127.0.0.1.
		if err := client.Auth(smtp.PlainAuth("", "panel", "gizli", host)); err != nil {
			t.Fatalf("control: net/smtp refused after all: %v", err)
		}
		attempts := s.authAttempts()
		if len(attempts) != 1 {
			t.Fatalf("control: the server received %d AUTH commands, want 1", len(attempts))
		}
		if !strings.HasPrefix(strings.ToUpper(attempts[0]), "AUTH PLAIN ") {
			t.Fatalf("control: unexpected AUTH line %q", attempts[0])
		}
		t.Logf("control: net/smtp sent the password in the clear as %q", attempts[0])
	})
}

// A relay that asks for nothing still works without encryption. This is
// the case the strict rule above must not break: a local Postfix on
// 127.0.0.1:25 is a real deployment, and there is no secret in the
// handshake to expose.
func TestProbeAnonymousRelay(t *testing.T) {
	s := startServer(t, serverConfig{offerSTARTTLS: false})

	p := s.config("", "").Probe()
	if p.Err != nil {
		t.Fatalf("probe failed: %v", p.Err)
	}
	if !p.Anonymous || p.Authenticated {
		t.Errorf("anonymous=%v authenticated=%v, want anonymous", p.Anonymous, p.Authenticated)
	}
	if p.Encrypted() {
		t.Error("Encrypted() is true on a plaintext connection")
	}
	if got := p.Diagnose(); got != DiagOK {
		t.Errorf("diagnosis = %q, want DiagOK", got)
	}
	if !p.OK() {
		t.Error("OK() is false for an anonymous relay that answered")
	}
}

func TestProbeOAuthOnly(t *testing.T) {
	tests := []struct {
		name       string
		mechanisms string
		password   string
		want       Diagnosis
	}{
		// Correct credentials on purpose in the first two: the server
		// refuses them because the mechanism is one it will not run,
		// which is exactly what a Microsoft 365 tenant with basic auth
		// disabled does. No password would have worked.
		{"only oauth", "XOAUTH2", "gizli", DiagNeedsOAuth},
		{"oauth and bearer", "XOAUTH2 OAUTHBEARER", "gizli", DiagNeedsOAuth},
		// A tenant that still allows basic auth advertises both. Then a
		// refusal is an ordinary refusal and saying "needs OAuth" would
		// send somebody into their tenant settings over a typo.
		{"oauth alongside plain, wrong password", "PLAIN XOAUTH2", "yanlis", DiagBadCredentials},
		// And the same server with the right password just works, which
		// is the half that proves WantsOAuth is not simply always false.
		{"oauth alongside plain, right password", "PLAIN XOAUTH2", "gizli", DiagOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startServer(t, serverConfig{
				offerSTARTTLS:  true,
				authMechanisms: tc.mechanisms,
				acceptUser:     "panel", acceptPass: "gizli",
			})
			p := s.config("panel", tc.password).Probe()
			if got := p.Diagnose(); got != tc.want {
				t.Errorf("diagnosis = %q, want %q (offered %v, err %v)", got, tc.want, p.AuthOffered, p.Err)
			}
		})
	}
}

func TestProbeBadCredentials(t *testing.T) {
	s := startServer(t, serverConfig{
		offerSTARTTLS:  true,
		authMechanisms: "PLAIN",
		acceptUser:     "panel", acceptPass: "gizli",
	})

	p := s.config("panel", "yanlis").Probe()
	if p.Err == nil {
		t.Fatal("probe succeeded with the wrong password")
	}
	if got := p.Diagnose(); got != DiagBadCredentials {
		t.Errorf("diagnosis = %q, want DiagBadCredentials", got)
	}
	// The server's own reply, kept apart from our sentence about it.
	if p.ServerCode != 535 {
		t.Errorf("server code = %d, want 535", p.ServerCode)
	}
	if !strings.Contains(p.ServerSaid, "authentication failed") {
		t.Errorf("server said %q, want the server's own words", p.ServerSaid)
	}
	if p.Detail != "" {
		t.Errorf("detail = %q, want empty for a failure that was an SMTP reply", p.Detail)
	}
	// The password did leave this machine here, and that is correct: the
	// connection was encrypted. Asserted so the refusal test above cannot
	// pass by the AUTH path being broken for everybody.
	if got := len(s.authAttempts()); got != 1 {
		t.Errorf("server saw %d AUTH commands, want 1", got)
	}
}

// The two ways a port and an encryption mode can disagree. Both used to
// be reported as something else - the first as a timeout, the second as
// "unreachable" - and both are one setting away from working.
func TestProbeWrongPort(t *testing.T) {
	t.Run("plaintext client, TLS-only port", func(t *testing.T) {
		s := startServer(t, serverConfig{silentGreeting: true})

		cfg := s.config("panel", "gizli")
		cfg.Encryption = EncryptionSTARTTLS
		cfg.Timeout = 700 * time.Millisecond

		p := cfg.Probe()
		if p.Err == nil {
			t.Fatal("probe succeeded against a silent port")
		}
		if !p.Reached {
			t.Error("Reached is false, but the connection was accepted")
		}
		if p.Stage != StageGreeting {
			t.Errorf("stage = %q, want %q", p.Stage, StageGreeting)
		}
		if got := p.Diagnose(); got != DiagWrongPort {
			t.Errorf("diagnosis = %q, want DiagWrongPort", got)
		}
	})

	t.Run("TLS client, plaintext port", func(t *testing.T) {
		s := startServer(t, serverConfig{offerSTARTTLS: true, authMechanisms: "PLAIN"})

		cfg := s.config("panel", "gizli")
		cfg.Encryption = EncryptionImplicit // the mismatch

		p := cfg.Probe()
		if p.Err == nil {
			t.Fatal("implicit TLS succeeded against a plaintext port")
		}
		if !p.Reached {
			t.Error("Reached is false, but TCP connected fine - only TLS failed")
		}
		if p.Stage != StageTLS {
			t.Errorf("stage = %q, want %q", p.Stage, StageTLS)
		}
		if got := p.Diagnose(); got != DiagWrongPort {
			t.Errorf("diagnosis = %q, want DiagWrongPort (detail %q)", got, p.Detail)
		}
	})
}

// The price of never skipping certificate verification, paid where it can
// be seen. A self-signed mail server fails here, and the panel says which
// failure it was instead of leaving somebody re-typing their password.
func TestProbeUntrustedCertificate(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		name := "starttls"
		if implicit {
			name = "implicit"
		}
		t.Run(name, func(t *testing.T) {
			s := startServer(t, serverConfig{
				implicitTLS:    implicit,
				offerSTARTTLS:  !implicit,
				authMechanisms: "PLAIN",
				untrustedCert:  true,
			})

			p := s.config("panel", "gizli").Probe()
			if p.Err == nil {
				t.Fatal("probe accepted an untrusted certificate")
			}
			if p.Stage != StageTLS {
				t.Errorf("stage = %q, want %q", p.Stage, StageTLS)
			}
			if got := p.Diagnose(); got != DiagTLSFailed {
				t.Errorf("diagnosis = %q, want DiagTLSFailed (detail %q)", got, p.Detail)
			}
			if got := len(s.authAttempts()); got != 0 {
				t.Errorf("server saw %d AUTH commands after a failed handshake, want 0", got)
			}
		})
	}
}

func TestProbeUnreachable(t *testing.T) {
	// A port that is definitely closed: bind one, learn its number, and
	// give it back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()

	cfg := Config{
		Host: "127.0.0.1", Port: addr.Port,
		Encryption: EncryptionSTARTTLS,
		Username:   "panel", Password: "gizli",
		Timeout: 2 * time.Second,
	}
	p := cfg.Probe()
	if p.Err == nil {
		t.Fatal("probe succeeded against a closed port")
	}
	if p.Reached {
		t.Error("Reached is true for a refused connection")
	}
	if got := p.Diagnose(); got != DiagUnreachable {
		t.Errorf("diagnosis = %q, want DiagUnreachable", got)
	}
}

func TestSuggestedPort(t *testing.T) {
	tests := []struct {
		port    int
		enc     Encryption
		want    int
		wantEnc Encryption
		wantOK  bool
	}{
		{587, EncryptionSTARTTLS, 465, EncryptionImplicit, true},
		{25, EncryptionSTARTTLS, 465, EncryptionImplicit, true},
		{465, EncryptionImplicit, 587, EncryptionSTARTTLS, true},
		// Unset means the default for the mode, so the suggestion is the
		// other one rather than nothing. The earlier version compared
		// c.Port directly and gave up on every account that had left the
		// port blank - which is most of them.
		{0, EncryptionSTARTTLS, 465, EncryptionImplicit, true},
		{0, EncryptionImplicit, 587, EncryptionSTARTTLS, true},
		{2525, EncryptionSTARTTLS, 0, "", false},
	}
	for _, tc := range tests {
		c := Config{Host: "smtp.example.com", Port: tc.port, Encryption: tc.enc}
		port, enc, ok := c.SuggestedPort()
		if port != tc.want || enc != tc.wantEnc || ok != tc.wantOK {
			t.Errorf("Config{Port:%d, Encryption:%q}.SuggestedPort() = (%d, %q, %v), want (%d, %q, %v)",
				tc.port, tc.enc, port, enc, ok, tc.want, tc.wantEnc, tc.wantOK)
		}
	}
}

func TestDefaultPort(t *testing.T) {
	if got := DefaultPort(EncryptionImplicit); got != 465 {
		t.Errorf("implicit default = %d, want 465", got)
	}
	if got := DefaultPort(EncryptionSTARTTLS); got != 587 {
		t.Errorf("starttls default = %d, want 587", got)
	}
	// An empty mode is STARTTLS, so an unset configuration lands on 587
	// rather than on 25.
	if got := DefaultPort(""); got != 587 {
		t.Errorf("unset default = %d, want 587", got)
	}
}

func TestAddr(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{Config{Host: "smtp.example.com", Port: 587}, "smtp.example.com:587"},
		{Config{Host: "smtp.example.com", Encryption: EncryptionImplicit}, "smtp.example.com:465"},
		{Config{Host: "smtp.example.com"}, "smtp.example.com:587"},
		// IPv6 has to come back bracketed or the dial fails.
		{Config{Host: "::1", Port: 587}, "[::1]:587"},
	}
	for _, tc := range tests {
		if got := tc.cfg.Addr(); got != tc.want {
			t.Errorf("Addr() = %q, want %q", got, tc.want)
		}
	}
}
