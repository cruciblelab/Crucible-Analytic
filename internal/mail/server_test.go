package mail

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real SMTP server, speaking the real protocol over a real socket.
//
// A mock would let this package's tests agree with this package's
// assumptions, which is the failure this project keeps finding. Every
// situation the diagnosis distinguishes is a specific sequence of
// server replies, so the only way to know the diagnosis is right is to
// have a server actually reply that way.
//
// Small enough to read: EHLO, STARTTLS, AUTH, MAIL, RCPT, DATA, QUIT.

// serverConfig describes how the fake server misbehaves.
type serverConfig struct {
	// implicitTLS wraps the listener in TLS, the way port 465 does.
	implicitTLS bool
	// offerSTARTTLS advertises the extension. False reproduces a server
	// that cannot encrypt, where net/smtp refuses to send the password.
	offerSTARTTLS bool
	// authMechanisms is advertised verbatim after "AUTH ". Empty
	// advertises no AUTH extension at all.
	authMechanisms string
	// acceptAuth is the credential the server accepts, base64 of the
	// PLAIN payload. Empty rejects everything.
	acceptUser, acceptPass string
	// refuseSender, refuseRecipient and refuseData reproduce the three
	// post-authentication rejections that actually happen.
	refuseSender, refuseRecipient, refuseData bool
	// silentGreeting accepts the connection and never says anything,
	// which is what plain TCP to a TLS-only port looks like.
	silentGreeting bool
	// untrustedCert withholds the server's certificate from the client's
	// trust store, reproducing a self-signed mail server.
	untrustedCert bool
}

type fakeServer struct {
	t    *testing.T
	cfg  serverConfig
	ln   net.Listener
	cert tls.Certificate

	mu       sync.Mutex
	received []string // full message bodies accepted
	auths    []string // raw AUTH command lines, in order
}

// startServer runs a fake SMTP server until the test ends, and points
// the package's TLS configuration at its certificate authority.
//
// The client verifies the certificate properly against that authority
// rather than skipping verification. Two reasons: it exercises the real
// handshake, which is the thing under test; and it keeps
// InsecureSkipVerify out of the package that handles a mail password,
// where a line copied from a test into the production path would hand
// that password to whoever answered.
//
// (internal/fullproxy's test does use InsecureSkipVerify against its own
// self-signed certificate. That is a test of a proxy rather than of a
// credential, and it is worth knowing the difference rather than
// claiming the repository has none.)
func startServer(t *testing.T, cfg serverConfig) *fakeServer {
	t.Helper()

	s := &fakeServer{t: t, cfg: cfg, cert: selfSigned(t)}

	// An empty pool when the certificate is meant to be untrusted: a
	// deterministic UnknownAuthorityError, rather than depending on
	// whatever the machine's system roots happen to contain.
	pool := x509.NewCertPool()
	if !cfg.untrustedCert {
		leaf, parseErr := x509.ParseCertificate(s.cert.Certificate[0])
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		pool.AddCert(leaf)
	}

	previous := tlsConfigFor
	tlsConfigFor = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	t.Cleanup(func() { tlsConfigFor = previous })

	var ln net.Listener
	var err error
	if cfg.implicitTLS {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
			Certificates: []tls.Certificate{s.cert},
		})
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return s
}

// addr is the host and port to point a Config at.
func (s *fakeServer) addr() (host string, port int) {
	h, p, _ := net.SplitHostPort(s.ln.Addr().String())
	var n int
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return h, n
}

// config builds a Config aimed at this server. Verification stays on;
// startServer has already made the test certificate trusted.
func (s *fakeServer) config(user, pass string) Config {
	host, port := s.addr()
	enc := EncryptionSTARTTLS
	if s.cfg.implicitTLS {
		enc = EncryptionImplicit
	}
	return Config{
		Host: host, Port: port, Encryption: enc,
		Username: user, Password: pass,
		From:    "panel@" + host,
		Timeout: 5 * time.Second,
	}
}

func (s *fakeServer) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.received...)
}

// authAttempts is every AUTH command line the server received.
//
// The measurement behind the central claim of this package: when it says
// the password was not sent, this is empty. Asserting on the client's own
// probe would only prove that the client believes its own comment.
func (s *fakeServer) authAttempts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

// offers reports whether the server advertised this AUTH mechanism.
func (s *fakeServer) offers(mechanism string) bool {
	for _, m := range strings.Fields(s.cfg.authMechanisms) {
		if strings.EqualFold(m, mechanism) {
			return true
		}
	}
	return false
}

func (s *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if s.cfg.silentGreeting {
		// Accept and say nothing, which is what a TLS-only port does to
		// a plaintext client. Held for longer than any test's own
		// timeout, so what the client observes is its deadline expiring
		// rather than this server giving up first - the second would
		// test the fake instead of the code.
		time.Sleep(9 * time.Second)
		return
	}

	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	write("220 test.local ESMTP fake")

	var authed bool
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, rest, _ := strings.Cut(line, " ")

		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			write("250-test.local")
			if s.cfg.offerSTARTTLS {
				write("250-STARTTLS")
			}
			if s.cfg.authMechanisms != "" {
				write("250-AUTH " + s.cfg.authMechanisms)
			}
			write("250 8BITMIME")

		case "STARTTLS":
			if !s.cfg.offerSTARTTLS {
				write("502 not implemented")
				continue
			}
			write("220 ready")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			r = bufio.NewReader(conn)
			write = func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

		case "AUTH":
			s.mu.Lock()
			s.auths = append(s.auths, line)
			s.mu.Unlock()

			mech, payload, _ := strings.Cut(rest, " ")
			// A real server refuses a mechanism it did not advertise, and
			// this has to be reproduced rather than assumed: net/smtp's
			// plainAuth.Start ignores the advertised list entirely
			// (smtp.go passes it in, auth.go never reads it), so the
			// client sends AUTH PLAIN to a server offering only XOAUTH2.
			// A fake that accepted it would authenticate happily against
			// a Microsoft 365 tenant that would have refused.
			if !s.offers(mech) {
				write("504 5.7.4 unrecognized authentication type")
				continue
			}
			if !strings.EqualFold(mech, "PLAIN") {
				write("504 5.7.4 mechanism not implemented by this fake")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				write("501 bad payload")
				continue
			}
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.cfg.acceptUser && parts[2] == s.cfg.acceptPass &&
				s.cfg.acceptUser != "" {
				authed = true
				write("235 authenticated")
			} else {
				write("535 5.7.8 authentication failed")
			}

		case "MAIL":
			if s.cfg.refuseSender {
				write("553 5.7.1 sender address not allowed")
				continue
			}
			_ = authed
			write("250 ok")

		case "RCPT":
			if s.cfg.refuseRecipient {
				write("550 5.1.1 recipient rejected")
				continue
			}
			write("250 ok")

		case "DATA":
			write("354 send it")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				// Undo the dot-stuffing a sender must apply, the way a
				// real server does. Without this the test would read
				// back a body that is not the one that was sent.
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				body.WriteString(l)
			}
			if s.cfg.refuseData {
				// Rejected at the end of DATA, which is where a size
				// limit or a content filter answers.
				write("552 5.3.4 message too large")
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, body.String())
			s.mu.Unlock()
			write("250 2.0.0 accepted")

		case "QUIT":
			write("221 bye")
			return

		default:
			write("500 unknown")
		}
	}
}

// selfSigned makes a certificate the test client will accept for
// 127.0.0.1.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Self-signed and used as its own authority, so the client can
		// verify it rather than skip verification.
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
