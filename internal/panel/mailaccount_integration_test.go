//go:build integration

package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/mail"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

const mailNS = "panel-posta"

func mailStore(t *testing.T) *Store {
	t.Helper()
	return newTestStore(t, mailNS)
}

func mailKey(t *testing.T) sealed.Key {
	t.Helper()
	var raw [sealed.KeySize]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	key, err := sealed.ParseKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func goodMailInput() MailAccountInput {
	return MailAccountInput{
		Host:        "smtp.ornek.com",
		Port:        587,
		Encryption:  mail.EncryptionSTARTTLS,
		Username:    "panel@ornek.com",
		Password:    "cok-gizli-bir-sifre",
		FromAddress: "panel@ornek.com",
		FromName:    "Crucible Analytic",
		Enabled:     true,
	}
}

func TestMailAccountRoundTrip(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	// Nothing configured is not an error. A fresh deployment has no mail
	// account and every page it has must still render.
	acc, err := s.MailAccount(ctx, key)
	if err != nil {
		t.Fatalf("MailAccount on an empty table: %v", err)
	}
	if acc.Configured {
		t.Fatal("reported an account before one was saved")
	}
	if _, err := s.MailConfig(ctx, key); !errors.Is(err, ErrNoMailAccount) {
		t.Errorf("MailConfig = %v, want ErrNoMailAccount", err)
	}

	in := goodMailInput()
	if err := s.SaveMailAccount(ctx, key, in, 0); err != nil {
		t.Fatalf("SaveMailAccount: %v", err)
	}

	acc, err = s.MailAccount(ctx, key)
	if err != nil {
		t.Fatalf("MailAccount: %v", err)
	}
	if !acc.Configured || acc.Host != in.Host || acc.Port != in.Port {
		t.Errorf("read back %+v", acc)
	}
	if !acc.HasPassword || acc.PasswordUnreadable {
		t.Errorf("HasPassword=%v PasswordUnreadable=%v, want a readable stored password",
			acc.HasPassword, acc.PasswordUnreadable)
	}

	cfg, err := s.MailConfig(ctx, key)
	if err != nil {
		t.Fatalf("MailConfig: %v", err)
	}
	if cfg.Password != in.Password {
		t.Errorf("password came back as %q", cfg.Password)
	}
	if cfg.Host != in.Host || cfg.Encryption != in.Encryption || cfg.From != in.FromAddress {
		t.Errorf("MailConfig = %+v", cfg)
	}
}

// The password must be unreadable in the row itself. Asserted by looking
// at the column directly rather than through the store, because the
// store is the thing under test - a Seal that had become a no-op would
// round-trip perfectly through its own API.
func TestMailPasswordIsNotStoredInTheClear(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	in := goodMailInput()
	in.Password = "TahminEdilemezBirSifre-9271"
	if err := s.SaveMailAccount(ctx, key, in, 0); err != nil {
		t.Fatalf("SaveMailAccount: %v", err)
	}

	var stored string
	if err := s.pool.QueryRow(ctx,
		`SELECT password_sealed FROM panel_smtp WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" {
		t.Fatal("nothing was stored")
	}
	if strings.Contains(stored, in.Password) {
		t.Fatalf("the password is in the column as typed: %s", stored)
	}

	// And nowhere else in the row either. A future column added by
	// somebody copying a line - a "hint", a "last used username" - is
	// how a password ends up somewhere nobody was looking.
	var wholeRow string
	if err := s.pool.QueryRow(ctx,
		`SELECT panel_smtp::text FROM panel_smtp WHERE id = 1`).Scan(&wholeRow); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wholeRow, in.Password) {
		t.Fatalf("the password appears somewhere in the row: %s", wholeRow)
	}
}

// A key that changed must be reported, not treated as a wrong password.
// This is the difference between "somebody edited panel.toml" and "your
// provider rejected these credentials", and sending somebody to the
// second when the first is true costs an afternoon.
func TestMailAccountWithAChangedKey(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	if err := s.SaveMailAccount(ctx, key, goodMailInput(), 0); err != nil {
		t.Fatalf("SaveMailAccount: %v", err)
	}

	other := mailKey(t)

	acc, err := s.MailAccount(ctx, other)
	if err != nil {
		t.Fatalf("MailAccount with a different key: %v", err)
	}
	if !acc.HasPassword {
		t.Error("HasPassword is false; a password is stored, it just cannot be read")
	}
	if !acc.PasswordUnreadable {
		t.Error("PasswordUnreadable is false with a key that cannot open it")
	}
	// Everything else on the page is still true and must still be
	// readable - the account did not stop existing.
	if acc.Host != "smtp.ornek.com" {
		t.Errorf("host = %q", acc.Host)
	}

	// And sending must refuse rather than try with an empty password,
	// which would come back as an authentication failure and point at
	// the wrong thing.
	cfg, err := s.MailConfig(ctx, other)
	if err == nil {
		t.Fatal("MailConfig succeeded with a key that cannot open the password")
	}
	if !errors.Is(err, sealed.ErrCannotOpen) {
		t.Errorf("err = %v, want sealed.ErrCannotOpen", err)
	}
	if cfg.Password != "" {
		t.Error("a password came back from a failed open")
	}
}

// Editing the sender name must not erase the password. The form shows no
// password, so an empty field is the ordinary case rather than a request
// to remove one - and a page that silently cleared it would break
// sending on the day somebody fixed a typo in their display name.
func TestSavingWithoutAPasswordKeepsTheStoredOne(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	first := goodMailInput()
	if err := s.SaveMailAccount(ctx, key, first, 0); err != nil {
		t.Fatalf("SaveMailAccount: %v", err)
	}

	second := goodMailInput()
	second.Password = ""
	second.FromName = "Crucible Analytic — Ölçüm"
	if err := s.SaveMailAccount(ctx, key, second, 0); err != nil {
		t.Fatalf("second SaveMailAccount: %v", err)
	}

	cfg, err := s.MailConfig(ctx, key)
	if err != nil {
		t.Fatalf("MailConfig: %v", err)
	}
	if cfg.Password != first.Password {
		t.Errorf("password is now %q, want the one stored before", cfg.Password)
	}
	if cfg.FromName != second.FromName {
		t.Errorf("the display name did not change: %q", cfg.FromName)
	}
}

// Removing a password has to be asked for explicitly.
func TestClearPasswordRemovesIt(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	if err := s.SaveMailAccount(ctx, key, goodMailInput(), 0); err != nil {
		t.Fatalf("SaveMailAccount: %v", err)
	}

	cleared := goodMailInput()
	cleared.Password = ""
	cleared.ClearPassword = true
	if err := s.SaveMailAccount(ctx, key, cleared, 0); err != nil {
		t.Fatalf("clearing: %v", err)
	}

	acc, err := s.MailAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if acc.HasPassword {
		t.Error("HasPassword is still true after ClearPassword")
	}
	cfg, err := s.MailConfig(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "" {
		t.Errorf("password = %q after clearing", cfg.Password)
	}
}

// Saving changes the account, so the last verification is now about a
// different account and must not be shown as if it were about this one.
func TestSavingClearsTheLastVerification(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	if err := s.SaveMailAccount(ctx, key, goodMailInput(), 0); err != nil {
		t.Fatal(err)
	}
	ok := mail.Probe{Reached: true, Authenticated: true, TLS: mail.EncryptionSTARTTLS, Stage: mail.StageDone}
	if err := s.RecordMailVerification(ctx, ok); err != nil {
		t.Fatalf("RecordMailVerification: %v", err)
	}

	acc, err := s.MailAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !acc.VerifiedOK || acc.VerifiedAt.IsZero() {
		t.Fatalf("verification not recorded: %+v", acc)
	}

	moved := goodMailInput()
	moved.Host = "smtp.baskasunucu.com"
	if err := s.SaveMailAccount(ctx, key, moved, 0); err != nil {
		t.Fatal(err)
	}

	acc, err = s.MailAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if acc.VerifiedOK || !acc.VerifiedAt.IsZero() {
		t.Errorf("the old verification survived a change of server: %+v", acc)
	}
}

// A failed verification is recorded with its diagnosis, which is the
// point of storing it at all: "why did nobody get the invitation" is
// asked weeks later, and the answer has to be in the row.
func TestRecordMailVerificationKeepsTheDiagnosis(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	if err := s.SaveMailAccount(ctx, key, goodMailInput(), 0); err != nil {
		t.Fatal(err)
	}

	failed := mail.Probe{
		Reached: true, TLS: mail.EncryptionSTARTTLS, Stage: mail.StageAuth,
		AuthOffered: []string{"PLAIN"},
		ServerCode:  535, ServerSaid: "5.7.8 authentication failed",
		Err: errors.New("rejected"),
	}
	if err := s.RecordMailVerification(ctx, failed); err != nil {
		t.Fatal(err)
	}

	acc, err := s.MailAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if acc.VerifiedOK {
		t.Error("a failed probe was recorded as OK")
	}
	if acc.VerifiedDiagnosis != mail.DiagBadCredentials {
		t.Errorf("diagnosis = %q, want %q", acc.VerifiedDiagnosis, mail.DiagBadCredentials)
	}
	if !strings.Contains(acc.VerifiedServerSaid, "535") ||
		!strings.Contains(acc.VerifiedServerSaid, "authentication failed") {
		t.Errorf("server said = %q, want the code and the reply", acc.VerifiedServerSaid)
	}
}

// A disabled account must not be sendable. Enforced in the read rather
// than left to every caller to remember, because "check Enabled first" is
// the kind of rule that holds until the third caller.
func TestDisabledAccountCannotSend(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	off := goodMailInput()
	off.Enabled = false
	if err := s.SaveMailAccount(ctx, key, off, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := s.MailConfig(ctx, key); !errors.Is(err, ErrNoMailAccount) {
		t.Errorf("MailConfig on a disabled account = %v, want ErrNoMailAccount", err)
	}
	// But the page still shows it, switched off, with its settings
	// intact - otherwise turning mail off would look like losing it.
	acc, err := s.MailAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !acc.Configured || acc.Enabled {
		t.Errorf("account = %+v, want configured and disabled", acc)
	}
}

// With no key configured, a password cannot be saved at all. The refusal
// is the point: the alternative is one plaintext password in a column
// everyone afterwards assumes is encrypted.
func TestNoKeyRefusesToStoreAPassword(t *testing.T) {
	s := mailStore(t)
	ctx := context.Background()

	var noKey sealed.Key
	err := s.SaveMailAccount(ctx, noKey, goodMailInput(), 0)
	if !errors.Is(err, sealed.ErrNoKey) {
		t.Fatalf("err = %v, want sealed.ErrNoKey", err)
	}

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM panel_smtp`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows were written despite the refusal", n)
	}

	// An account with no credentials - a local relay - is still allowed
	// without a key, because there is no secret to protect.
	relay := goodMailInput()
	relay.Username, relay.Password = "", ""
	relay.Host, relay.Port = "127.0.0.1", 25
	if err := s.SaveMailAccount(ctx, noKey, relay, 0); err != nil {
		t.Fatalf("an anonymous relay was refused with no key: %v", err)
	}
}

// The single-row constraint is real, not a convention. Without it a
// second row is a legal insert and the panel would send through
// whichever one came back first.
func TestOnlyOneMailAccountRowIsPossible(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	if err := s.SaveMailAccount(ctx, key, goodMailInput(), 0); err != nil {
		t.Fatal(err)
	}
	second := goodMailInput()
	second.Host = "smtp.ikinci.com"
	if err := s.SaveMailAccount(ctx, key, second, 0); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM panel_smtp`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows in panel_smtp, want 1", n)
	}

	// And the database refuses a second one written around the store.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO panel_smtp (id, host, port, encryption, from_address)
		VALUES (2, 'smtp.ucuncu.com', 587, 'starttls', 'x@ornek.com')`); err == nil {
		t.Error("the database accepted a second mail account row")
	}
}

func TestDeleteMailAccount(t *testing.T) {
	s, key := mailStore(t), mailKey(t)
	ctx := context.Background()

	if err := s.SaveMailAccount(ctx, key, goodMailInput(), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMailAccount(ctx); err != nil {
		t.Fatalf("DeleteMailAccount: %v", err)
	}
	acc, err := s.MailAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Configured {
		t.Error("the account survived deletion")
	}
	// Deleting twice is not an error: the operator's intent is "there
	// should be no mail account", and it is satisfied either way.
	if err := s.DeleteMailAccount(ctx); err != nil {
		t.Errorf("deleting an absent account: %v", err)
	}
}

func TestMailAccountInputValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*MailAccountInput)
		wantErr bool
	}{
		{"good", func(*MailAccountInput) {}, false},
		{"no host", func(in *MailAccountInput) { in.Host = "" }, true},
		{"whitespace in host", func(in *MailAccountInput) { in.Host = "smtp ornek.com" }, true},
		{"port zero", func(in *MailAccountInput) { in.Port = 0 }, true},
		{"port too high", func(in *MailAccountInput) { in.Port = 70000 }, true},
		{"unknown encryption", func(in *MailAccountInput) { in.Encryption = "none" }, true},
		{"no sender", func(in *MailAccountInput) { in.FromAddress = "" }, true},
		{"sender is not an address", func(in *MailAccountInput) { in.FromAddress = "panel" }, true},
		// The same rule internal/mail applies, reached through the same
		// function - so a sender the page accepts is a sender that sends.
		{"sender with a line break", func(in *MailAccountInput) {
			in.FromAddress = "panel@ornek.com\r\nBcc: x@y.com"
		}, true},
		{"sender with a display name", func(in *MailAccountInput) {
			in.FromAddress = "Panel <panel@ornek.com>"
		}, false},
		{"no username is fine", func(in *MailAccountInput) { in.Username = "" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := goodMailInput()
			tc.mutate(&in)
			err := in.Validate()
			if tc.wantErr && err == nil {
				t.Error("accepted an input that should have been refused")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("refused a valid input: %v", err)
			}
		})
	}
}
