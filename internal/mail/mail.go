// Package mail sends the panel's occasional messages, and - more
// importantly - explains why it could not.
//
// # Why the diagnosis is the point
//
// Email is not load-bearing here. Password reset works through recovery
// codes and an operator link (C7.2); an invitation works through a link
// (C7.1). Both were built without email on purpose and both still work
// without it. So this package adds a convenience, and the rule that
// follows is that a convenience must never break the thing it is a
// convenience for.
//
// What actually goes wrong with SMTP is never "sending failed". It is
// one of a handful of specific, fixable situations that all surface as
// the same unhelpful sentence:
//
//   - the provider requires OAuth and this cannot speak it
//   - the server never offered STARTTLS, so the password was not sent
//   - the port is 465, which needs TLS from the first byte
//   - the certificate does not verify
//   - the credentials are right and the sender address is not allowed
//
// Telling somebody "authentication failed" when the truth is "your
// provider turned off SMTP passwords in 2023 and you need an app
// password" is the difference between a five-minute setup and an
// abandoned one. So Probe returns everything it learned rather than an
// error, and the caller shows it.
//
// # The one rule: a password never crosses an unencrypted connection
//
// This is stricter than net/smtp, deliberately. PlainAuth sends the
// password in the clear whenever the host is "localhost", "127.0.0.1" or
// "::1" (net/smtp/auth.go, isLocalhost). The exemption is defensible -
// those bytes stay on the machine - but it is invisible, it is decided by
// a string in a configuration file rather than by where the packets go,
// and it makes one of this package's own answers untrue: "the password
// was never sent" would be a lie for exactly the local relay somebody is
// most likely to misconfigure.
//
// So this package decides before net/smtp does. No encryption and
// credentials configured means the AUTH command is never issued, and
// DiagNoTLS then means what it says, every time, with no exception to
// remember.
//
// A relay that needs no credentials still works unencrypted, because
// there is no secret to expose in the handshake and a local Postfix on
// 127.0.0.1:25 is a real deployment. The wizard says the connection is
// unencrypted in that case rather than hiding it.
//
// # What this deliberately does not do
//
// No third-party email API. A self-hosted, privacy-first product routing
// its customers' addresses through somebody else's service - and adding
// an account and a bill to the installation - would contradict the
// reason it is self-hosted.
//
// No DKIM signing. Every real SMTP provider signs already, and half a
// DKIM implementation is worse than trusting theirs.
//
// No OAuth. net/smtp offers PLAIN and CRAM-MD5 and nothing else, and
// pretending otherwise would mean shipping an XOAUTH2 implementation
// nobody here can test against a real tenant. Instead Probe *recognises*
// a server that wants OAuth and says so by name.
package mail

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Encryption is how the connection is protected.
type Encryption string

const (
	// EncryptionSTARTTLS connects in the clear and upgrades. Port 587.
	EncryptionSTARTTLS Encryption = "starttls"
	// EncryptionImplicit is TLS from the first byte. Port 465.
	//
	// net/smtp's own SendMail cannot do this: it calls Dial, which is
	// plain TCP, so the only path it offers is STARTTLS. Implicit TLS
	// needs a TLS client over the connection plus smtp.NewClient, which
	// is why this package builds the connection itself rather than
	// calling SendMail.
	//
	// Worth supporting rather than declaring 587 the one true port:
	// plenty of hosting providers offer 465 and some offer only 465.
	EncryptionImplicit Encryption = "implicit"
)

// DefaultPort is the port for an encryption mode.
func DefaultPort(e Encryption) int {
	if e == EncryptionImplicit {
		return 465
	}
	// 587 rather than 25: most providers block outbound 25 from a VDS,
	// and 25 is for server-to-server relay rather than submission.
	return 587
}

// Config is one SMTP account.
type Config struct {
	Host       string
	Port       int
	Encryption Encryption
	Username   string
	Password   string
	// From is the envelope sender and the From: header. Often but not
	// always the same as Username - a provider may authenticate as one
	// mailbox and allow sending as another.
	From string
	// FromName is the display name. Optional.
	FromName string
	// Timeout bounds the whole conversation. A wizard's verify button is
	// somebody waiting at a screen, so this is short by default.
	Timeout time.Duration
}

// Addr is host:port with the default filled in.
func (c Config) Addr() string {
	port := c.Port
	if port == 0 {
		port = DefaultPort(c.Encryption)
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// Stage is how far a probe got before it stopped.
//
// Named stages rather than a bare error, because which stage failed is
// most of the diagnosis: the same "connection refused" means a firewall
// at StageConnect and a mid-conversation drop at StageAuth.
//
// The three send stages are separate for a reason worth stating. The
// first version of Diagnose told a refused sender from a refused
// recipient by searching the server's reply text for "sender" and
// "recipient". Servers write that text however they like and some write
// it in their own language, so the answer depended on wording nobody
// here controls. The stage is a fact about which command this package
// issued, so it cannot be wrong.
type Stage string

const (
	StageConnect   Stage = "baglanti"
	StageTLS       Stage = "tls"
	StageGreeting  Stage = "karsilama"
	StageAuth      Stage = "kimlik"
	StageSender    Stage = "gonderen"
	StageRecipient Stage = "alici"
	StageData      Stage = "icerik"
	StageDone      Stage = "tamam"
)

// Probe is everything one connection attempt learned.
//
// Fields rather than an error, because every one of these is worth
// showing even when the attempt succeeded: the AUTH list explains what a
// provider supports, and the TLS state is the difference between a
// password that was protected and one that was never sent.
type Probe struct {
	// Reached is whether TCP connected at all.
	Reached bool
	// Greeting is the server's banner, which usually names the software
	// and sometimes the hosting provider.
	Greeting string
	// TLS is how the conversation ended up encrypted, empty when it did
	// not. This is the field to trust for "is this connection private",
	// in both encryption modes.
	TLS Encryption
	// TLSOffered is whether the server advertised the STARTTLS extension.
	// Only meaningful in STARTTLS mode - an implicit-TLS connection is
	// already encrypted and has no reason to advertise it.
	TLSOffered bool
	// AuthOffered is the mechanisms the server advertised, verbatim.
	AuthOffered []string
	// Authenticated is whether AUTH succeeded. False when no AUTH was
	// attempted, including the anonymous case below.
	Authenticated bool
	// Anonymous is whether the account has no credentials, so no AUTH was
	// attempted and none was needed. Kept apart from Authenticated
	// because a screen that says "authenticated" about a connection where
	// nothing was ever checked is telling the reader something untrue.
	Anonymous bool
	// Sent is whether a message was accepted.
	Sent bool
	// Stage is how far it got.
	Stage Stage
	// ServerSaid is the server's own reply text, set only when the
	// failure really was an SMTP reply. Kept separate from the diagnosis
	// so the panel can show both: what it said, and what that means.
	ServerSaid string
	// ServerCode is the SMTP reply code that came with it, 0 when the
	// failure was not a reply.
	ServerCode int
	// Detail is the technical detail of a local failure - a TLS
	// handshake, a timeout, a refused connection. Separate from
	// ServerSaid so neither field ever attributes to the server something
	// the server never said.
	Detail string
	// Err is the failure, if any.
	Err error
}

// OK reports whether the probe got far enough to be usable: the
// credentials checked out, or there were none to check.
func (p Probe) OK() bool { return p.Err == nil && (p.Authenticated || p.Anonymous || p.Sent) }

// Encrypted reports whether the conversation was private.
func (p Probe) Encrypted() bool { return p.TLS != "" }

// WantsOAuth reports that the server advertises an OAuth mechanism and
// none this package can speak.
//
// The situation behind most "authentication failed" reports from
// Microsoft 365 and Google Workspace: basic SMTP auth is disabled for
// the tenant, so the only mechanism offered is XOAUTH2. No amount of
// checking the password helps, and the fix is a tenant setting or an app
// password rather than anything in this panel.
func (p Probe) WantsOAuth() bool {
	var oauth, basic bool
	for _, m := range p.AuthOffered {
		switch strings.ToUpper(m) {
		case "XOAUTH2", "OAUTHBEARER":
			oauth = true
		case "PLAIN", "LOGIN", "CRAM-MD5":
			basic = true
		}
	}
	return oauth && !basic
}

// ErrNotConfigured means no SMTP account has been set up. Not a failure:
// the panel works without one.
var ErrNotConfigured = errors.New("mail: no SMTP account is configured")

// ErrWouldSendPasswordInClear is the refusal described in the package
// comment: credentials are configured and the connection is not
// encrypted, so no AUTH command was issued.
var ErrWouldSendPasswordInClear = errors.New("mail: the connection is not encrypted, so the password was not sent")

// tlsConfigFor builds the client TLS configuration for a host.
//
// A package variable so the tests can trust their own certificate
// authority, and deliberately NOT a field on Config.
//
// There is no setting anywhere in this product that turns certificate
// verification off, and that absence is the design. "Skip verification"
// is the single most common way TLS ends up disabled in production: it
// gets switched on to get past a self-signed certificate during setup and
// is never switched back, and the result looks exactly like a working
// encrypted connection. A mail password sent over an unverified
// connection is a mail password handed to whoever answered.
//
// The cost of that choice is real and is paid here rather than hidden: a
// self-signed mail server fails, so DiagTLSFailed exists to say exactly
// that and point at the fix, instead of leaving somebody staring at
// "authentication failed".
//
// The tests replace this to trust the certificate their own server
// presents, which verifies the real handshake rather than skipping it -
// and leaves this package with no InsecureSkipVerify in it at all.
//
// (internal/fullproxy's test does use InsecureSkipVerify against its own
// self-signed certificate. That is a test of a proxy rather than of a
// credential, and it is worth knowing the difference rather than
// claiming the repository has none.)
var tlsConfigFor = func(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// note records a failure, keeping what the server said apart from what
// happened locally.
//
// *textproto.Error is net/smtp's carrier for a real SMTP reply, so this
// is how the code tells "the server refused" from "we never got there" -
// by type, rather than by matching message text that changes between
// servers and between languages.
func (p *Probe) note(err error) {
	var te *textproto.Error
	if errors.As(err, &te) {
		p.ServerCode = te.Code
		p.ServerSaid = te.Msg
		return
	}
	p.Detail = err.Error()
}

// Probe connects, negotiates TLS, and authenticates - without sending
// anything. It is what the wizard's verify button calls.
func (c Config) Probe() Probe {
	client, p, err := c.dial()
	if err != nil {
		return p
	}
	defer client.Close()

	if err := c.authenticate(client, &p); err != nil {
		return p
	}
	_ = client.Quit()
	p.Stage = StageDone
	return p
}

// dial gets as far as an authenticated-capable client: TCP, TLS, EHLO.
//
// TCP first in both encryption modes, including implicit TLS. The
// earlier version called tls.DialWithDialer, which does both at once and
// therefore cannot say which one failed: pointing implicit TLS at a
// plaintext port came back as "unreachable" when the truth was that the
// connection worked perfectly and the port was wrong.
func (c Config) dial() (*smtp.Client, Probe, error) {
	p := Probe{Stage: StageConnect}

	enc := c.Encryption
	if enc == "" {
		enc = EncryptionSTARTTLS
	}

	dialer := &net.Dialer{Timeout: c.timeout()}
	conn, err := dialer.Dial("tcp", c.Addr())
	if err != nil {
		p.Err = fmt.Errorf("mail: connecting to %s: %w", c.Addr(), err)
		p.note(err)
		return nil, p, p.Err
	}
	p.Reached = true

	// One deadline for the whole conversation. A wizard is somebody
	// waiting at a screen, so the useful bound is on the total, not on
	// each round trip.
	_ = conn.SetDeadline(time.Now().Add(c.timeout()))

	if enc == EncryptionImplicit {
		p.Stage = StageTLS
		tlsConn := tls.Client(conn, tlsConfigFor(c.Host))
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			p.Err = fmt.Errorf("mail: TLS handshake with %s: %w", c.Addr(), err)
			p.note(err)
			return nil, p, p.Err
		}
		conn = tlsConn
		p.TLS = EncryptionImplicit
	}

	p.Stage = StageGreeting
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		p.Err = fmt.Errorf("mail: the server did not greet us: %w", err)
		p.note(err)
		return nil, p, p.Err
	}

	if enc != EncryptionImplicit {
		p.Stage = StageTLS
		p.TLSOffered, _ = client.Extension("STARTTLS")
		if p.TLSOffered {
			if err := client.StartTLS(tlsConfigFor(c.Host)); err != nil {
				client.Close()
				p.Err = fmt.Errorf("mail: STARTTLS with %s: %w", c.Addr(), err)
				p.note(err)
				return nil, p, p.Err
			}
			p.TLS = EncryptionSTARTTLS
		}
	}

	if ok, mechanisms := client.Extension("AUTH"); ok {
		p.AuthOffered = strings.Fields(mechanisms)
	}
	return client, p, nil
}

// authenticate runs AUTH, filling in the probe either way.
func (c Config) authenticate(client *smtp.Client, p *Probe) error {
	p.Stage = StageAuth

	if c.Username == "" && c.Password == "" {
		// A server that needs no credentials - a local relay, usually.
		// Nothing was checked, so this is not authentication, and the
		// probe says so by name rather than by claiming success.
		p.Anonymous = true
		return nil
	}

	// The rule from the package comment, applied before net/smtp gets a
	// chance to apply its own weaker one. See there for why the localhost
	// exemption is not inherited.
	if p.TLS == "" {
		p.Err = ErrWouldSendPasswordInClear
		return p.Err
	}

	auth := smtp.PlainAuth("", c.Username, c.Password, c.Host)
	if err := client.Auth(auth); err != nil {
		p.Err = fmt.Errorf("mail: authenticating as %s: %w", c.Username, err)
		p.note(err)
		return p.Err
	}
	p.Authenticated = true
	return nil
}
