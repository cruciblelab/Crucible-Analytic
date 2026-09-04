package ui

import (
	"io/fs"
	"path"
	"regexp"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// One name for the developer password field.
//
// # The defect this exists because of
//
// There were two. devgate.FromRequest reads the constant
// devgate.FormField; the health page and the settings page rendered
// `name="gelistirici_parolasi"` instead. Settings worked anyway,
// because settings.go read that literal itself. The health page did
// not: upgradePost calls devgate.RequestFrom, which reads the constant,
// so the password a person typed into the upgrade form reached nothing.
//
// The consequence was worse than a dead field. A locked deployment
// answered a *correct* password with "Yükseltme geliştirici parolasına
// kilitli. Parolayı yazarak başlatabilirsiniz." - it told the person to
// do the thing they had just done, and would have gone on telling them
// forever.
//
// It survived because nothing posted that form with a password. The
// store's own tests built a devgate.Authorization directly, which is
// the right unit test and cannot see a form; the page tests never
// pressed the button. Between them was a field name, in a template,
// that nobody compared to anything.
//
// *Bir alanı doldurup göndermeyen bir test, alanın adını sınamaz.*
//
// # Why the check reads the constant
//
// A test naming "developer_password" as a literal would be a third
// place the name lives, and the next rename would leave it agreeing
// with nothing. It is derived from devgate.FormField, so renaming the
// constant renames what this test demands.

var templatePasswordInput = regexp.MustCompile(`<input[^>]*type="password"[^>]*>`)
var inputName = regexp.MustCompile(`name="([^"]+)"`)

// signInFields are the password inputs that are not the developer gate:
// somebody's own account password, which goes to the session layer
// rather than to devgate.
//
// Named, not pattern-matched. A rule loose enough to be convenient is a
// rule that silently absorbs the case it was meant to catch - and the
// case it was meant to catch is exactly "a password field whose name
// nothing reads".
var signInFields = map[string]string{
	"parola":             "the account password: sign-in, and the one chosen when an invitation is claimed",
	"parola_tekrar":      "the repeat of that chosen password",
	"mevcut_parola":      "somebody's current account password, on their own account page",
	"yeni_parola":        "a new account password, set from the account page or a recovery code",
	"yeni_parola_tekrar": "the repeat of that new password",
	"sifre":              "the SMTP account's password, which goes to the mail store encrypted and never to devgate",
}

// TestEveryDeveloperPasswordFieldUsesTheNameTheGateReads.
func TestEveryDeveloperPasswordFieldUsesTheNameTheGateReads(t *testing.T) {
	var checked int
	err := fs.WalkDir(templateFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".html" {
			return nil
		}
		body, readErr := fs.ReadFile(templateFS, p)
		if readErr != nil {
			t.Errorf("reading %s: %v", p, readErr)
			return nil
		}
		for _, tag := range templatePasswordInput.FindAllString(string(body), -1) {
			m := inputName.FindStringSubmatch(tag)
			if m == nil {
				t.Errorf("%s has a password input with no name at all: %s", p, tag)
				continue
			}
			checked++
			name := m[1]
			if name == devgate.FormField {
				continue
			}
			if why, ok := signInFields[name]; ok {
				_ = why
				continue
			}
			t.Errorf("%s posts a password as %q. devgate.FromRequest reads %q, so this "+
				"one reaches nothing - and a gate that never sees the password refuses "+
				"a correct one while telling the person to type it. If this field is "+
				"genuinely not the developer gate's, add it to signInFields with a "+
				"reason", p, name, devgate.FormField)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no password inputs found in any template, so this test would pass by " +
			"examining nothing")
	}
}
