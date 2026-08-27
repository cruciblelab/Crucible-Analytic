package mail

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
)

// Message is one outgoing email.
type Message struct {
	To      string
	Subject string
	// Body is plain text. No HTML, and that is a decision rather than an
	// omission: everything this panel sends is a sentence and a link,
	// HTML would double the surface for a templating mistake to become an
	// injection, and a plain-text message renders the same everywhere.
	Body string
}

// ErrInvalidAddress means an address would not parse. Its own error
// because the wizard's test-message field is operator input, and "that
// address is not valid" is a different thing to tell somebody than
// "the server refused the recipient".
var ErrInvalidAddress = errors.New("mail: the address is not a valid email address")

// Send delivers one message and reports what happened at every stage.
//
// A Probe rather than an error, for the same reason Probe exists: the
// panel must never say only "sent". It says what it sent, to whom, and
// what the server answered - because the failure mode this guards
// against is a message that vanishes while the screen says it went.
func (c Config) Send(m Message) Probe {
	// Addresses first, before anything is opened. An address that cannot
	// be sent is not worth a TCP connection, and finding out locally
	// gives a precise answer instead of whatever the server would have
	// replied.
	from := c.From
	if from == "" {
		from = c.Username
	}
	from, err := parseAddress(from)
	if err != nil {
		return Probe{Stage: StageSender, Err: fmt.Errorf("mail: sender %q: %w", c.From, err), Detail: err.Error()}
	}
	to, err := parseAddress(m.To)
	if err != nil {
		return Probe{Stage: StageRecipient, Err: fmt.Errorf("mail: recipient %q: %w", m.To, err), Detail: err.Error()}
	}

	client, p, err := c.dial()
	if err != nil {
		return p
	}
	defer client.Close()

	if err := c.authenticate(client, &p); err != nil {
		return p
	}

	p.Stage = StageSender
	if err := client.Mail(from); err != nil {
		p.Err = fmt.Errorf("mail: the server refused %s as the sender: %w", from, err)
		p.note(err)
		return p
	}

	p.Stage = StageRecipient
	if err := client.Rcpt(to); err != nil {
		p.Err = fmt.Errorf("mail: the server refused %s as a recipient: %w", to, err)
		p.note(err)
		return p
	}

	p.Stage = StageData
	w, err := client.Data()
	if err != nil {
		p.Err = fmt.Errorf("mail: starting the message body: %w", err)
		p.note(err)
		return p
	}
	if _, err := w.Write(c.compose(from, to, m)); err != nil {
		p.Err = fmt.Errorf("mail: writing the message: %w", err)
		p.note(err)
		return p
	}
	if err := w.Close(); err != nil {
		// The close is where the server accepts or rejects the whole
		// message, so this is the one that carries a bounce reason.
		p.Err = fmt.Errorf("mail: the server rejected the message: %w", err)
		p.note(err)
		return p
	}

	_ = client.Quit()
	p.Sent = true
	p.Stage = StageDone
	return p
}

// parseAddress reduces an address to its bare mailbox form.
//
// net/mail's parser accepts "Ali Veli <ali@example.com>" and returns the
// address inside it, which is convenient - somebody pasting a name and
// address into the wizard gets what they meant. It also rejects, by
// construction, anything carrying a line break, so no separate check for
// header injection is needed here: an address that could open a header
// of its own never parses.
func parseAddress(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ErrInvalidAddress
	}
	parsed, err := mail.ParseAddress(v)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}
	if parsed.Address == "" || !strings.Contains(parsed.Address, "@") {
		return "", ErrInvalidAddress
	}
	return parsed.Address, nil
}

// compose builds the RFC 5322 message.
//
// Nothing here is concatenated into a header without an encoder in
// front of it, and that - rather than any stripping - is what makes the
// headers safe:
//
//   - both addresses arrive already parsed, so neither can carry a line
//     break;
//   - FromName goes through mail.Address.String, which encodes the whole
//     display name as an RFC 2047 word the moment it contains a byte
//     below a space, CR and LF included;
//   - the subject goes through mime.QEncoding, which does the same, and
//     which a Turkish subject needs anyway to survive the trip.
//
// The first version also ran FromName through a strip-the-newlines
// helper. Removing that helper changed no test result, which is how it
// was found: mail.Address.String already guaranteed it, so the strip was
// deleting characters from display names for no benefit while implying
// in the code that it was load-bearing. The tests that pin these
// guarantees are in send_test.go and they fail if any of the three
// encoders is replaced with concatenation.
func (c Config) compose(from, to string, m Message) []byte {
	var b strings.Builder
	if c.FromName != "" {
		b.WriteString("From: " + (&mail.Address{Name: c.FromName, Address: from}).String() + "\r\n")
	} else {
		b.WriteString("From: " + from + "\r\n")
	}
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", m.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: " + messageID(from) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	// Auto-Submitted tells a well-behaved mail system not to send an
	// out-of-office reply to a password reset, and marks the message as
	// machine-generated for anything that files mail by category.
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeBody(m.Body))
	b.WriteString("\r\n")
	return []byte(b.String())
}

// normalizeBody puts the body in CRLF form and protects the dot.
//
// A line consisting of a single dot ends the DATA command, so a body
// containing one would truncate the message there and leave the rest to
// be parsed as SMTP commands. net/textproto's DotWriter - which
// smtp.Client.Data returns - already does this stuffing, so this is
// belt and braces; it is here because the cost is one line and the
// failure it guards against is a message that silently loses its ending.
func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	return body
}

// sanitizeHeader strips anything that could start a new header line.
//
// One caller: the Message-ID below. Everything else written into a
// header here passes through an encoder that neutralises line breaks on
// the way, and duplicating that would be a guard that never fires. The
// Message-ID is the exception - it is assembled by hand and written
// raw - so this is the mechanism there rather than a second copy of one.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// messageID builds a globally unique Message-ID.
//
// Some spam filters score a missing or malformed Message-ID, and a
// deliverability problem this package could have avoided is the worst
// kind: invisible, intermittent, and blamed on the wrong thing.
func messageID(from string) string {
	var raw [16]byte
	// rand.Read from crypto/rand never fails on any platform Go
	// supports; ignoring it here rather than failing a send over an
	// impossible condition.
	_, _ = rand.Read(raw[:])
	id := base64.RawURLEncoding.EncodeToString(raw[:])

	domain := "localhost"
	if at := strings.LastIndex(from, "@"); at >= 0 && at+1 < len(from) {
		domain = from[at+1:]
	}
	return "<" + id + "@" + sanitizeHeader(domain) + ">"
}
