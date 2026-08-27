package mail

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// timedOut is the error a connection deadline actually produces.
//
// The real one rather than a hand-written stand-in. The first version of
// this file defined its own net.Error and got it wrong - it left out
// Temporary(), which the interface still requires, so errors.As never
// matched it and three cases here failed against correct code. A stub
// that has to be right about an interface is one more thing that can be
// wrong; os.ErrDeadlineExceeded is what dial and SetDeadline hand back,
// so there is nothing left to get wrong.
var timedOut = os.ErrDeadlineExceeded

// The table covers every Diagnosis, including the ones the SMTP server
// tests already reach. Duplicated on purpose: those tests prove the
// diagnosis comes out of a real conversation, this one pins what each
// shape of Probe means, and the completeness check below only works if
// the whole family is in one place.
var diagnosisCases = []struct {
	name  string
	probe Probe
	want  Diagnosis
}{
	{
		name:  "authenticated",
		probe: Probe{Reached: true, TLS: EncryptionSTARTTLS, Authenticated: true, Stage: StageDone},
		want:  DiagOK,
	},
	{
		name:  "anonymous relay",
		probe: Probe{Reached: true, Anonymous: true, Stage: StageDone},
		want:  DiagOK,
	},
	{
		name:  "sent",
		probe: Probe{Reached: true, TLS: EncryptionSTARTTLS, Authenticated: true, Sent: true, Stage: StageDone},
		want:  DiagOK,
	},
	{
		name:  "connection refused",
		probe: Probe{Stage: StageConnect, Err: errors.New("connection refused")},
		want:  DiagUnreachable,
	},
	{
		// A firewall that drops rather than rejects. Distinguished from a
		// refusal because the advice differs: a refusal means nothing is
		// listening, a drop usually means a rule in the way.
		name:  "connection timed out",
		probe: Probe{Stage: StageConnect, Err: fmt.Errorf("dialing: %w", timedOut)},
		want:  DiagTimeout,
	},
	{
		name:  "no greeting",
		probe: Probe{Reached: true, Stage: StageGreeting, Err: fmt.Errorf("reading banner: %w", timedOut)},
		want:  DiagWrongPort,
	},
	{
		name: "TLS record header from a plaintext server",
		probe: Probe{Reached: true, Stage: StageTLS,
			Err: fmt.Errorf("handshake: %w", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})},
		want: DiagWrongPort,
	},
	{
		name: "untrusted certificate",
		probe: Probe{Reached: true, Stage: StageTLS,
			Err: fmt.Errorf("handshake: %w", x509.UnknownAuthorityError{})},
		want: DiagTLSFailed,
	},
	{
		name: "certificate issued for another name",
		probe: Probe{Reached: true, Stage: StageTLS,
			Err: fmt.Errorf("handshake: %w", x509.HostnameError{Host: "smtp.ornek.com"})},
		want: DiagTLSFailed,
	},
	{
		// Not a certificate problem and not a plaintext port either -
		// a protocol version or cipher mismatch. Still a TLS failure,
		// and still not something to report as a bad password.
		name:  "handshake refused for another reason",
		probe: Probe{Reached: true, Stage: StageTLS, Err: errors.New("tls: protocol version not supported")},
		want:  DiagTLSFailed,
	},
	{
		name:  "TLS handshake timed out",
		probe: Probe{Reached: true, Stage: StageTLS, Err: fmt.Errorf("handshake: %w", timedOut)},
		want:  DiagTimeout,
	},
	{
		name: "no encryption available",
		probe: Probe{Reached: true, Stage: StageAuth, TLSOffered: false,
			AuthOffered: []string{"PLAIN"}, Err: ErrWouldSendPasswordInClear},
		want: DiagNoTLS,
	},
	{
		name: "only OAuth offered",
		probe: Probe{Reached: true, Stage: StageAuth, TLS: EncryptionSTARTTLS,
			AuthOffered: []string{"XOAUTH2"}, Err: errors.New("504 unrecognized")},
		want: DiagNeedsOAuth,
	},
	{
		name: "OAuth offered alongside PLAIN",
		probe: Probe{Reached: true, Stage: StageAuth, TLS: EncryptionSTARTTLS,
			AuthOffered: []string{"PLAIN", "XOAUTH2"}, Err: errors.New("535 rejected")},
		want: DiagBadCredentials,
	},
	{
		name: "password rejected",
		probe: Probe{Reached: true, Stage: StageAuth, TLS: EncryptionSTARTTLS,
			AuthOffered: []string{"PLAIN"}, ServerCode: 535, Err: errors.New("535 rejected")},
		want: DiagBadCredentials,
	},
	{
		name: "authentication timed out",
		probe: Probe{Reached: true, Stage: StageAuth, TLS: EncryptionSTARTTLS,
			AuthOffered: []string{"PLAIN"}, Err: fmt.Errorf("auth: %w", timedOut)},
		want: DiagTimeout,
	},
	{
		name: "sender refused",
		probe: Probe{Reached: true, Stage: StageSender, TLS: EncryptionSTARTTLS,
			Authenticated: true, ServerCode: 553, Err: errors.New("553 not allowed")},
		want: DiagSenderRefused,
	},
	{
		name: "recipient refused",
		probe: Probe{Reached: true, Stage: StageRecipient, TLS: EncryptionSTARTTLS,
			Authenticated: true, ServerCode: 550, Err: errors.New("550 no such user")},
		want: DiagRecipientRefused,
	},
	{
		name: "message refused",
		probe: Probe{Reached: true, Stage: StageData, TLS: EncryptionSTARTTLS,
			Authenticated: true, ServerCode: 552, Err: errors.New("552 too large")},
		want: DiagMessageRejected,
	},
	{
		name:  "recipient address does not parse",
		probe: Probe{Stage: StageRecipient, Err: fmt.Errorf("recipient: %w", ErrInvalidAddress)},
		want:  DiagInvalidAddress,
	},
	{
		// Nothing above fits. The panel shows the server's own words
		// rather than inventing an explanation for them.
		name: "something else entirely",
		probe: Probe{Reached: true, Stage: StageDone, TLS: EncryptionSTARTTLS,
			Authenticated: true, Err: errors.New("connection reset by peer")},
		want: DiagOther,
	},
}

func TestDiagnose(t *testing.T) {
	for _, tc := range diagnosisCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.probe.Diagnose(); got != tc.want {
				t.Errorf("Diagnose() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEveryDiagnosisIsCovered reads the constants out of diagnose.go and
// requires the table above to name every one of them.
//
// Read from the source rather than kept as a hand-written list, which is
// the difference between a mirror that cannot drift and one that drifts
// the first time somebody is in a hurry. A new Diagnosis with no case
// here fails this test, and the failure names the constant - so the
// question "what does the panel say for this one" gets asked while the
// person who added it is still looking at it, rather than the first time
// somebody hits it in production.
func TestEveryDiagnosisIsCovered(t *testing.T) {
	declared := declaredDiagnoses(t)
	if len(declared) < 5 {
		t.Fatalf("only %d Diagnosis constants found in diagnose.go; the scan is broken, not the code", len(declared))
	}

	covered := make(map[Diagnosis]bool)
	for _, tc := range diagnosisCases {
		covered[tc.want] = true
	}

	for name, value := range declared {
		if !covered[value] {
			t.Errorf("%s (%q) is never produced by any case in diagnosisCases", name, value)
		}
	}
}

// declaredDiagnoses returns every Diag* constant in diagnose.go, by name
// and by value.
func declaredDiagnoses(t *testing.T) map[string]Diagnosis {
	t.Helper()

	src, err := os.ReadFile("diagnose.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "diagnose.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string]Diagnosis)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "Diagnosis" {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				out[name.Name] = Diagnosis(lit.Value[1 : len(lit.Value)-1])
			}
		}
	}
	return out
}

// The values are stored nowhere and shown nowhere raw - the panel looks
// up a sentence by them - but they are keys, and two constants sharing a
// key would silently make one of them unreachable in the catalog.
func TestDiagnosisValuesAreDistinct(t *testing.T) {
	seen := make(map[Diagnosis]string)
	for name, value := range declaredDiagnoses(t) {
		if other, clash := seen[value]; clash {
			t.Errorf("%s and %s share the value %q", name, other, value)
		}
		seen[value] = name
	}
}

func TestProbeOKAndEncrypted(t *testing.T) {
	tests := []struct {
		name          string
		probe         Probe
		wantOK        bool
		wantEncrypted bool
	}{
		{"authenticated over TLS", Probe{Authenticated: true, TLS: EncryptionSTARTTLS}, true, true},
		{"anonymous in the clear", Probe{Anonymous: true}, true, false},
		{"authenticated but errored afterwards", Probe{Authenticated: true, TLS: EncryptionImplicit, Err: errors.New("x")}, false, true},
		{"nothing happened", Probe{}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.probe.OK(); got != tc.wantOK {
				t.Errorf("OK() = %v, want %v", got, tc.wantOK)
			}
			if got := tc.probe.Encrypted(); got != tc.wantEncrypted {
				t.Errorf("Encrypted() = %v, want %v", got, tc.wantEncrypted)
			}
		})
	}
}

func TestWantsOAuth(t *testing.T) {
	tests := []struct {
		mechanisms []string
		want       bool
	}{
		{[]string{"XOAUTH2"}, true},
		{[]string{"OAUTHBEARER"}, true},
		{[]string{"XOAUTH2", "OAUTHBEARER"}, true},
		{[]string{"PLAIN", "XOAUTH2"}, false},
		{[]string{"LOGIN", "XOAUTH2"}, false},
		{[]string{"CRAM-MD5", "OAUTHBEARER"}, false},
		{[]string{"PLAIN"}, false},
		{nil, false},
		// Advertised mechanisms are not case-normalised by the server,
		// and a server that writes them in lower case is not a server
		// that wants something different.
		{[]string{"xoauth2"}, true},
		{[]string{"plain", "xoauth2"}, false},
		// A mechanism nobody here knows is neither OAuth nor basic. Not
		// claiming it needs OAuth is the point: the honest answer for an
		// unknown mechanism is the ordinary credential failure, not a
		// confident wrong explanation.
		{[]string{"GSSAPI"}, false},
	}
	for _, tc := range tests {
		p := Probe{AuthOffered: tc.mechanisms}
		if got := p.WantsOAuth(); got != tc.want {
			t.Errorf("WantsOAuth(%v) = %v, want %v", tc.mechanisms, got, tc.want)
		}
	}
}
