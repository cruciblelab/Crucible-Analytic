package web

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/mail"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// TestEveryDiagnosisHasWords is the mirror internal/panel/ui cannot
// write.
//
// That package deliberately knows nothing about the domain: it renders,
// it does not decide, so it holds a hand-written list of the diagnosis
// values and checks nothing in the catalog is dead. This package imports
// both, so it can run the check that matters in the other direction -
// against the real constants.
//
// The failure this catches: somebody adds a Diagnosis, wires it into
// Diagnose, and ships. Nothing breaks at build time. Nothing breaks in
// any test that does not reach that exact SMTP situation. The first
// person to meet it is somebody whose mail is already not working, and
// the page that exists to name their problem shows them a blank line.
func TestEveryDiagnosisHasWords(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}

	declared := declaredDiagnosisValues(t)
	if len(declared) < 5 {
		t.Fatalf("only %d diagnoses found in internal/mail; the scan is broken, not the code", len(declared))
	}

	for _, lang := range catalogs.Languages() {
		for _, value := range declared {
			// DiagOK is the empty string and has no sentence: "nothing
			// to say" is what it means, and a key for it would be a
			// sentence nobody can reach.
			if value == string(mail.DiagOK) {
				continue
			}
			for _, prefix := range []string{"posta.tani.", "posta.oneri."} {
				key := prefix + value
				if !lang.Has(key) {
					t.Errorf("%s: %s has no words (T gives %q)", lang.Code, key, lang.T(key))
				}
			}
		}
	}
}

// And the same for the encryption modes, whose option labels are looked
// up from the value.
func TestEveryEncryptionModeHasALabel(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	modes := []mail.Encryption{mail.EncryptionSTARTTLS, mail.EncryptionImplicit}
	for _, lang := range catalogs.Languages() {
		for _, m := range modes {
			key := "posta.mod." + string(m)
			if !lang.Has(key) {
				t.Errorf("%s: %s has no label", lang.Code, key)
			}
		}
	}
}

// declaredDiagnosisValues reads the Diagnosis constants out of
// internal/mail's source.
//
// From the source rather than from a list here, for the same reason the
// scan in internal/mail/diagnose_test.go reads its own file: a
// hand-maintained copy is a copy that drifts, and the drift is silent in
// exactly the direction that matters - a new constant with no entry.
func declaredDiagnosisValues(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "..", "mail", "diagnose.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatal(err)
	}

	var out []string
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
			for i := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting %s: %v", lit.Value, err)
				}
				out = append(out, unquoted)
			}
		}
	}
	return out
}

// TestNoPasswordFieldReachesATemplate.
//
// The page's guarantee is structural rather than careful: there is no
// password on the types a template can address, so no template can print
// one even by ranging over the whole struct. This is the check that
// notices when that stops being true.
//
// The first draft of this test built a form from an account and searched
// it for a secret that had never been put there - a search that cannot
// fail, on a value that cannot contain it. It was replaced by reading
// the field names out of the source, which is the only form of this
// check that can actually go red.
func TestNoPasswordFieldReachesATemplate(t *testing.T) {
	// HasPassword and PasswordUnreadable are booleans about a password
	// rather than a password, and the page needs both: one fills in
	// "leave blank to keep the current password", the other warns that
	// the stored one no longer opens.
	allowed := map[string]bool{"haspassword": true, "passwordunreadable": true}

	for _, tc := range []struct{ file, typeName string }{
		{filepath.Join("..", "mailaccount.go"), "MailAccount"},
		{"mail.go", "mailForm"},
		{"mail.go", "mailProbeView"},
	} {
		for _, field := range structFieldNames(t, tc.file, tc.typeName) {
			lower := strings.ToLower(field)
			if !strings.Contains(lower, "password") && !strings.Contains(lower, "sifre") {
				continue
			}
			if allowed[lower] {
				continue
			}
			t.Errorf("%s gained a field named %q; the guarantee this page rests on is that "+
				"no type a template can address carries the password", tc.typeName, field)
		}
	}
}

// structFieldNames reads the field names of a type out of a source file.
func structFieldNames(t *testing.T, path, typeName string) []string {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != typeName {
			return true
		}
		structType, ok := spec.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				names = append(names, name.Name)
			}
		}
		return false
	})
	if len(names) == 0 {
		t.Fatalf("no fields found on %s; the scan is broken", typeName)
	}
	return names
}

func TestMailFormOfDefaultsForAFreshDeployment(t *testing.T) {
	form := mailFormOf(panel.MailAccount{})
	if form.Port != strconv.Itoa(mail.DefaultPort(mail.EncryptionSTARTTLS)) {
		t.Errorf("port = %q, want the STARTTLS default", form.Port)
	}
	if form.Encryption != string(mail.EncryptionSTARTTLS) {
		t.Errorf("encryption = %q, want starttls", form.Encryption)
	}
	// Everything else empty: a fresh deployment has nothing to show, and
	// a pre-filled server name would be a guess presented as a setting.
	if form.Host != "" || form.Username != "" || form.FromAddr != "" {
		t.Errorf("form = %+v, want empty fields", form)
	}
}

// The probe view is what turns a diagnosis into a screen, and its
// riskiest line is the one that decides where the text under "what the
// server said" comes from.
func TestMailProbeViewKeepsTheServerApartFromUs(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	lang := catalogs.Base()
	cfg := mail.Config{Host: "smtp.ornek.com", Port: 587, Encryption: mail.EncryptionSTARTTLS}

	t.Run("an SMTP reply is attributed to the server", func(t *testing.T) {
		view := mailProbeViewFor(lang, cfg, mail.Probe{
			Reached: true, TLS: mail.EncryptionSTARTTLS, Stage: mail.StageAuth,
			AuthOffered: []string{"PLAIN"},
			ServerCode:  535, ServerSaid: "5.7.8 authentication failed",
			Err: errNotAccepted,
		})
		if view.OK {
			t.Error("a failure was drawn as OK")
		}
		if !strings.HasPrefix(view.ServerSaid, "535 ") {
			t.Errorf("server said = %q, want the code in front", view.ServerSaid)
		}
		if view.Headline == "" || view.Advice == "" {
			t.Errorf("headline = %q, advice = %q", view.Headline, view.Advice)
		}
	})

	t.Run("a local failure is not", func(t *testing.T) {
		view := mailProbeViewFor(lang, cfg, mail.Probe{
			Stage:  mail.StageConnect,
			Detail: "dial tcp 10.0.0.1:587: connect: connection refused",
			Err:    errNotAccepted,
		})
		// The detail still shows - it is the most useful line on the
		// page - but it arrived through Detail rather than ServerSaid,
		// and nothing above it claims a server said it.
		if !strings.Contains(view.ServerSaid, "connection refused") {
			t.Errorf("the detail was lost: %q", view.ServerSaid)
		}
		if view.Reached {
			t.Error("Reached is true for a refused connection")
		}
	})

	t.Run("a wrong port offers the other one", func(t *testing.T) {
		view := mailProbeViewFor(lang, cfg, mail.Probe{
			Reached: true, Stage: mail.StageGreeting, Err: errNotAccepted,
		})
		if view.SuggestedPort != 465 {
			t.Errorf("suggested port = %d, want 465", view.SuggestedPort)
		}
		if view.SuggestedEncLabel == "" {
			t.Error("the suggestion has no label for the encryption mode")
		}
	})

	t.Run("success says so", func(t *testing.T) {
		view := mailProbeViewFor(lang, cfg, mail.Probe{
			Reached: true, TLS: mail.EncryptionSTARTTLS,
			Authenticated: true, Stage: mail.StageDone,
		})
		if !view.OK || view.Headline == "" {
			t.Errorf("view = %+v", view)
		}
		if view.Advice != "" {
			t.Errorf("advice = %q on a success", view.Advice)
		}
	})
}

// errNotAccepted stands in for whatever the library returned.
//
// Deliberately featureless. Diagnose reads the stage and the error's
// type, never its text, so a realistic message here would suggest the
// tests depend on one - and the day somebody changed a message to match
// what a real server says, they would learn nothing about whether it
// mattered.
var errNotAccepted = errors.New("refused")
