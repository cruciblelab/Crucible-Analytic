package mail

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
)

// Diagnosis names what went wrong in a way somebody can act on.
//
// The panel turns this into a sentence from its own catalogs; this
// package decides *which* sentence. The split matters: whether a refusal
// means "wrong password" or "your provider turned off SMTP passwords" is
// a question about SMTP, and it belongs beside the code that had the
// conversation. Which words to use for it is a question about the person
// reading, and that belongs in the panel.
type Diagnosis string

const (
	// DiagOK: nothing to say.
	DiagOK Diagnosis = ""
	// DiagUnreachable: TCP never connected. Usually a blocked port -
	// outbound 25 is blocked by most hosting providers, and some block
	// 587 too.
	DiagUnreachable Diagnosis = "ulasilamiyor"
	// DiagWrongPort: connected, and then the conversation failed in the
	// specific way an encryption mismatch fails. Both directions land
	// here: plaintext to a 465 port waits forever for a banner, and TLS
	// to a 587 port reads "220 ..." where a TLS record should be.
	DiagWrongPort Diagnosis = "yanlis_port"
	// DiagTLSFailed: encryption was attempted and the handshake was
	// rejected. Almost always the certificate - self-signed, expired, or
	// issued for a different name than the one configured.
	//
	// This one exists because this package never skips verification. A
	// product that did would show a working connection here; this one
	// shows the reason and the fix.
	DiagTLSFailed Diagnosis = "sertifika"
	// DiagNoTLS: the connection was not encrypted and credentials were
	// configured, so no AUTH command was issued. Reported as itself
	// rather than as an auth failure, because "wrong password" would be
	// false - the password never left this machine.
	DiagNoTLS Diagnosis = "sifreleme_yok"
	// DiagNeedsOAuth: the server offers only OAuth mechanisms. No
	// password will ever work; the fix is an app password or a tenant
	// setting.
	DiagNeedsOAuth Diagnosis = "oauth_gerekiyor"
	// DiagBadCredentials: the server rejected the username or password,
	// having offered a mechanism we can speak, over an encrypted link.
	DiagBadCredentials Diagnosis = "kimlik_reddedildi"
	// DiagSenderRefused: authentication worked and the server would not
	// accept this From address. Common when a provider authenticates one
	// mailbox and only allows sending as that mailbox.
	DiagSenderRefused Diagnosis = "gonderen_reddedildi"
	// DiagRecipientRefused: the server would not accept the recipient.
	DiagRecipientRefused Diagnosis = "alici_reddedildi"
	// DiagMessageRejected: sender and recipient were accepted and the
	// message itself was not - a size limit, a spam score, a content
	// filter. The server's own words matter most here, so the panel shows
	// them.
	DiagMessageRejected Diagnosis = "ileti_reddedildi"
	// DiagTimeout: it connected and then stopped answering.
	DiagTimeout Diagnosis = "zaman_asimi"
	// DiagInvalidAddress: an address would not parse, so nothing was
	// attempted. Not a server problem at all, and telling somebody the
	// server refused their recipient when the truth is a typo would send
	// them looking in the wrong place.
	DiagInvalidAddress Diagnosis = "gecersiz_adres"
	// DiagOther: something happened that none of the above describes.
	// The server's own words are shown instead of a guess.
	DiagOther Diagnosis = "diger"
)

// Diagnose reads a probe and names the situation.
//
// Driven by the stage that failed and by the *type* of the error, never
// by the text of a server's reply. Reply wording differs between
// implementations and some servers write it in their own language, so a
// diagnosis that reads it is a diagnosis that is right on the servers it
// was written against and wrong elsewhere.
//
// Within a stage, the most specific conclusion wins: a server that only
// offers OAuth is a more useful thing to say than "authentication
// failed", even though both are true and the second is what the library
// reported.
func (p Probe) Diagnose() Diagnosis {
	if p.Err == nil && (p.Authenticated || p.Anonymous || p.Sent) {
		return DiagOK
	}

	// Before anything about the network: this one is decided locally and
	// nothing was ever attempted, so the stage it carries describes which
	// address was wrong rather than how far a connection got.
	if errors.Is(p.Err, ErrInvalidAddress) {
		return DiagInvalidAddress
	}

	if !p.Reached {
		if isTimeout(p.Err) {
			return DiagTimeout
		}
		return DiagUnreachable
	}

	switch p.Stage {
	case StageTLS:
		// An error at this stage means a handshake was attempted and
		// failed. A server that simply did not offer STARTTLS does not
		// error here; it carries on to StageAuth with TLSOffered false.
		if p.Err == nil {
			break
		}
		// Reading a plaintext SMTP banner where a TLS record header
		// should be. A typed signal rather than a guess, and the exact
		// signature of implicit TLS aimed at a submission port.
		var recordErr tls.RecordHeaderError
		if errors.As(p.Err, &recordErr) {
			return DiagWrongPort
		}
		if isCertificateError(p.Err) {
			return DiagTLSFailed
		}
		if isTimeout(p.Err) {
			return DiagTimeout
		}
		return DiagTLSFailed

	case StageGreeting:
		// Reached, but the greeting never came. Plain TCP to a port
		// expecting TLS looks exactly like this: the connection opens and
		// then nothing that parses as SMTP arrives, because the server is
		// waiting for a ClientHello that is never coming.
		return DiagWrongPort

	case StageAuth:
		// The most useful thing first: no password can work here.
		if p.WantsOAuth() {
			return DiagNeedsOAuth
		}
		// Then the one that would otherwise be reported as a wrong
		// password, which it is not.
		if errors.Is(p.Err, ErrWouldSendPasswordInClear) {
			return DiagNoTLS
		}
		if isTimeout(p.Err) {
			return DiagTimeout
		}
		if p.Err != nil {
			return DiagBadCredentials
		}

	case StageSender:
		if p.Err != nil {
			return DiagSenderRefused
		}

	case StageRecipient:
		if p.Err != nil {
			return DiagRecipientRefused
		}

	case StageData:
		if p.Err != nil {
			return DiagMessageRejected
		}
	}

	if isTimeout(p.Err) {
		return DiagTimeout
	}
	if p.Err != nil {
		return DiagOther
	}
	return DiagOK
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// isCertificateError reports whether a TLS failure was about the
// certificate rather than about the transport.
//
// By type, for the same reason as everything else here: the message text
// of x509 errors has changed between Go releases, and a check that reads
// it would quietly stop working on an upgrade.
func isCertificateError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	return errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalid) ||
		errors.As(err, &verification)
}

// SuggestedPort is the port to try instead, when the diagnosis is that
// the encryption mode does not match the port.
//
// Returned as a suggestion rather than retried automatically: silently
// reconnecting somewhere else would leave the person with a working
// setup and no idea which port it is on, and the next person to read the
// configuration would find a number that was never typed.
func (c Config) SuggestedPort() (port int, encryption Encryption, ok bool) {
	port = c.Port
	if port == 0 {
		port = DefaultPort(c.Encryption)
	}
	switch port {
	case 587, 25:
		return 465, EncryptionImplicit, true
	case 465:
		return 587, EncryptionSTARTTLS, true
	}
	return 0, "", false
}
