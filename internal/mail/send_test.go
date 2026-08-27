package mail

import (
	"errors"
	"io"
	"mime"
	"net/mail"
	"strings"
	"testing"
)

func TestSendDelivers(t *testing.T) {
	s := startServer(t, serverConfig{
		offerSTARTTLS:  true,
		authMechanisms: "PLAIN",
		acceptUser:     "panel", acceptPass: "gizli",
	})

	cfg := s.config("panel", "gizli")
	cfg.From = "panel@ornek.com"
	cfg.FromName = "Crucible Analytic"

	p := cfg.Send(Message{
		To:      "sahip@ornek.com",
		Subject: "Parola sıfırlama bağlantınız",
		Body:    "Merhaba,\n\nBağlantı: https://ornek.com/sifirla?t=abc\n\nİyi günler.",
	})
	if p.Err != nil {
		t.Fatalf("send failed: %v", p.Err)
	}
	if !p.Sent || p.Stage != StageDone {
		t.Fatalf("sent=%v stage=%q, want a completed send", p.Sent, p.Stage)
	}

	got := s.messages()
	if len(got) != 1 {
		t.Fatalf("the server received %d messages, want 1", len(got))
	}

	msg, err := mail.ReadMessage(strings.NewReader(got[0]))
	if err != nil {
		t.Fatalf("the delivered message does not parse as RFC 5322: %v\n---\n%s", err, got[0])
	}

	// The display name and the address both survive, and the Turkish in
	// the name is encoded rather than sent as raw bytes in a header.
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		t.Fatalf("From does not parse: %v (%q)", err, msg.Header.Get("From"))
	}
	if from.Address != "panel@ornek.com" || from.Name != "Crucible Analytic" {
		t.Errorf("From = %q <%s>, want the configured name and address", from.Name, from.Address)
	}
	if got := msg.Header.Get("To"); got != "sahip@ornek.com" {
		t.Errorf("To = %q", got)
	}

	// The subject is Turkish, so it must arrive RFC 2047 encoded and
	// decode back to exactly what was asked for. Raw UTF-8 in a header is
	// the classic way a subject turns into mojibake at the far end.
	rawSubject := msg.Header.Get("Subject")
	if strings.Contains(rawSubject, "ı") {
		t.Errorf("Subject carries raw UTF-8: %q", rawSubject)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(rawSubject)
	if err != nil {
		t.Fatalf("Subject does not decode: %v", err)
	}
	if decoded != "Parola sıfırlama bağlantınız" {
		t.Errorf("Subject decoded to %q", decoded)
	}

	for _, h := range []struct{ name, want string }{
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "8bit"},
		{"Auto-Submitted", "auto-generated"},
	} {
		if got := msg.Header.Get(h.name); got != h.want {
			t.Errorf("%s = %q, want %q", h.name, got, h.want)
		}
	}
	if _, err := msg.Header.Date(); err != nil {
		t.Errorf("Date does not parse: %v (%q)", err, msg.Header.Get("Date"))
	}
	id := msg.Header.Get("Message-ID")
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") || !strings.Contains(id, "@ornek.com") {
		t.Errorf("Message-ID = %q, want <opaque@ornek.com>", id)
	}

	raw, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "https://ornek.com/sifirla?t=abc") {
		t.Errorf("the link is missing from the body:\n%s", body)
	}
	// Every line break in a message on the wire is CRLF. A bare LF is
	// accepted by many servers and mangled by some, which is the kind of
	// bug that only shows up at one recipient's provider.
	if strings.Contains(strings.ReplaceAll(body, "\r\n", ""), "\n") {
		t.Errorf("the body contains a bare LF:\n%q", body)
	}
}

// Two Message-IDs from two sends must differ. A fixed one gets messages
// deduplicated away by the receiving server, silently, at the second one.
func TestMessageIDIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := messageID("panel@ornek.com")
		if seen[id] {
			t.Fatalf("Message-ID repeated: %s", id)
		}
		seen[id] = true
	}
}

// The Message-ID is the one header assembled by hand and written without
// an encoder, so it carries its own guard. Tested at the function rather
// than through Send, because Send's callers can only reach it with an
// address that has already been parsed - the guard is there for the next
// caller, and this is the boundary where its contract can be stated.
func TestMessageIDCannotInjectHeaders(t *testing.T) {
	for _, from := range []string{
		"panel@ornek.com\r\nBcc: saldirgan@kotu.com",
		"panel@ornek.com\nBcc: saldirgan@kotu.com",
		"panel@\rornek.com",
	} {
		id := messageID(from)
		if strings.ContainsAny(id, "\r\n") {
			t.Errorf("messageID(%q) = %q, which contains a line break", from, id)
		}
		if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") {
			t.Errorf("messageID(%q) = %q, want <...>", from, id)
		}
	}
}

// A body line that is a single dot ends the DATA command. If it were sent
// unprotected the message would be truncated there and the rest read as
// SMTP commands.
func TestSendBodyWithLeadingDot(t *testing.T) {
	s := startServer(t, serverConfig{
		offerSTARTTLS:  true,
		authMechanisms: "PLAIN",
		acceptUser:     "panel", acceptPass: "gizli",
	})

	cfg := s.config("panel", "gizli")
	cfg.From = "panel@ornek.com"

	body := "birinci satir\n.\nson satir"
	p := cfg.Send(Message{To: "sahip@ornek.com", Subject: "nokta", Body: body})
	if p.Err != nil {
		t.Fatalf("send failed: %v", p.Err)
	}
	got := s.messages()
	if len(got) != 1 {
		t.Fatalf("the server received %d messages, want 1", len(got))
	}
	if !strings.Contains(got[0], "son satir") {
		t.Errorf("the message was truncated at the dot:\n%s", got[0])
	}
}

func TestSendRefusals(t *testing.T) {
	tests := []struct {
		name      string
		cfg       serverConfig
		wantStage Stage
		want      Diagnosis
		wantCode  int
	}{
		{
			name:      "sender refused",
			cfg:       serverConfig{refuseSender: true},
			wantStage: StageSender, want: DiagSenderRefused, wantCode: 553,
		},
		{
			name:      "recipient refused",
			cfg:       serverConfig{refuseRecipient: true},
			wantStage: StageRecipient, want: DiagRecipientRefused, wantCode: 550,
		},
		{
			name:      "message refused",
			cfg:       serverConfig{refuseData: true},
			wantStage: StageData, want: DiagMessageRejected, wantCode: 552,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.offerSTARTTLS = true
			cfg.authMechanisms = "PLAIN"
			cfg.acceptUser, cfg.acceptPass = "panel", "gizli"
			s := startServer(t, cfg)

			c := s.config("panel", "gizli")
			c.From = "panel@ornek.com"

			p := c.Send(Message{To: "sahip@ornek.com", Subject: "deneme", Body: "deneme"})
			if p.Err == nil {
				t.Fatal("send succeeded, want a refusal")
			}
			if p.Sent {
				t.Error("Sent is true after a refusal")
			}
			// Authentication worked, which is the point: these are the
			// failures that happen *after* the credentials are right, and
			// reporting them as a credential problem is what sends
			// somebody re-typing a password that was never wrong.
			if !p.Authenticated {
				t.Error("Authenticated is false, but the credentials were accepted")
			}
			if p.Stage != tc.wantStage {
				t.Errorf("stage = %q, want %q", p.Stage, tc.wantStage)
			}
			if got := p.Diagnose(); got != tc.want {
				t.Errorf("diagnosis = %q, want %q", got, tc.want)
			}
			// The diagnosis comes from the stage, not from the reply
			// text, but the reply is still carried through for the panel
			// to show underneath it.
			if p.ServerCode != tc.wantCode {
				t.Errorf("server code = %d, want %d", p.ServerCode, tc.wantCode)
			}
			if p.ServerSaid == "" {
				t.Error("ServerSaid is empty for a failure that was an SMTP reply")
			}
		})
	}
}

// A diagnosis that read the server's reply text instead of the stage
// would get these two backwards. Turkish because a Turkish-hosted mail
// server writes its refusals in Turkish, and English keyword matching
// would then answer confidently and wrongly.
func TestRefusalDiagnosisDoesNotDependOnReplyWording(t *testing.T) {
	sender := Probe{
		Reached: true, Authenticated: true, TLS: EncryptionSTARTTLS,
		Stage: StageSender, ServerCode: 553,
		ServerSaid: "gonderen adresine izin verilmiyor",
		Err:        errors.New("refused"),
	}
	if got := sender.Diagnose(); got != DiagSenderRefused {
		t.Errorf("sender diagnosis = %q, want DiagSenderRefused", got)
	}

	recipient := Probe{
		Reached: true, Authenticated: true, TLS: EncryptionSTARTTLS,
		Stage: StageRecipient, ServerCode: 550,
		ServerSaid: "boyle bir kullanici yok",
		Err:        errors.New("refused"),
	}
	if got := recipient.Diagnose(); got != DiagRecipientRefused {
		t.Errorf("recipient diagnosis = %q, want DiagRecipientRefused", got)
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"sahip@ornek.com", "sahip@ornek.com", false},
		{"  sahip@ornek.com  ", "sahip@ornek.com", false},
		{"Ali Veli <ali@ornek.com>", "ali@ornek.com", false},
		{`"Veli, Ali" <ali@ornek.com>`, "ali@ornek.com", false},
		{"", "", true},
		{"sahip", "", true},
		{"@ornek.com", "", true},
		// The header injection attempt: anything carrying a line break
		// fails to parse, so no separate stripping step is needed.
		{"sahip@ornek.com>\r\nBcc: baskasi@ornek.com", "", true},
		{"sahip@ornek.com\nBcc: baskasi@ornek.com", "", true},
	}
	for _, tc := range tests {
		got, err := parseAddress(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAddress(%q) = %q, want an error", tc.in, got)
			} else if !errors.Is(err, ErrInvalidAddress) {
				t.Errorf("parseAddress(%q) error = %v, want ErrInvalidAddress", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddress(%q): %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("parseAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An address that could inject a header must be refused before anything
// is opened - and the refusal must say "that address is wrong", not "the
// server refused your recipient", which would send somebody to look at a
// server that was never contacted.
func TestSendRejectsInjectedAddressBeforeConnecting(t *testing.T) {
	s := startServer(t, serverConfig{
		offerSTARTTLS:  true,
		authMechanisms: "PLAIN",
		acceptUser:     "panel", acceptPass: "gizli",
	})

	cfg := s.config("panel", "gizli")
	cfg.From = "panel@ornek.com"

	p := cfg.Send(Message{
		To:      "sahip@ornek.com>\r\nBcc: saldirgan@kotu.com",
		Subject: "deneme",
		Body:    "deneme",
	})
	if p.Err == nil {
		t.Fatal("send accepted an address containing a line break")
	}
	if !errors.Is(p.Err, ErrInvalidAddress) {
		t.Errorf("err = %v, want ErrInvalidAddress", p.Err)
	}
	if got := p.Diagnose(); got != DiagInvalidAddress {
		t.Errorf("diagnosis = %q, want DiagInvalidAddress", got)
	}
	if p.Reached {
		t.Error("a connection was opened for a message that could never be sent")
	}
	if got := len(s.messages()); got != 0 {
		t.Errorf("the server received %d messages, want 0", got)
	}
}

// A subject is written into a header by this package. QEncoding encodes
// every byte below a space, so a line break in a subject cannot open a
// header of its own - asserted here rather than assumed, because that
// property is the only thing standing between the subject and an
// injection and it lives in the standard library.
func TestComposeSubjectCannotInjectHeaders(t *testing.T) {
	c := Config{From: "panel@ornek.com"}
	out := string(c.compose("panel@ornek.com", "sahip@ornek.com", Message{
		Subject: "merhaba\r\nBcc: saldirgan@kotu.com",
		Body:    "deneme",
	}))

	headers, _, found := strings.Cut(out, "\r\n\r\n")
	if !found {
		t.Fatalf("no header/body separator in:\n%q", out)
	}
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("a Bcc header was injected through the subject:\n%s", headers)
		}
	}

	msg, err := mail.ReadMessage(strings.NewReader(out))
	if err != nil {
		t.Fatalf("the message does not parse: %v", err)
	}
	if got := msg.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc = %q, want none", got)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decoded, "Bcc: saldirgan@kotu.com") {
		t.Errorf("the subject lost its text instead of being encoded: %q", decoded)
	}
}

// FromName is the one header value that does not pass through an address
// parser. What protects it is mail.Address.String, which encodes the
// whole display name as an RFC 2047 word as soon as it contains a byte
// below a space - so this test pins a guarantee that lives in the
// standard library, and fires if anyone ever builds this header by
// concatenation instead.
func TestComposeFromNameCannotInjectHeaders(t *testing.T) {
	c := Config{FromName: "Panel\r\nBcc: saldirgan@kotu.com", From: "panel@ornek.com"}
	out := string(c.compose("panel@ornek.com", "sahip@ornek.com", Message{Subject: "deneme", Body: "deneme"}))

	msg, err := mail.ReadMessage(strings.NewReader(out))
	if err != nil {
		t.Fatalf("the message does not parse: %v", err)
	}
	if got := msg.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc = %q, want none", got)
	}
	if _, err := mail.ParseAddress(msg.Header.Get("From")); err != nil {
		t.Errorf("From does not parse: %v (%q)", err, msg.Header.Get("From"))
	}
}
