package web

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "panel.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	path := writeConfig(t, `panel_dsn = "postgres://panel@localhost/crucible"`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8090" {
		t.Errorf("listen_addr defaulted to %q; an admin surface must not bind publicly by omission", cfg.ListenAddr)
	}
	if !cfg.CookiesAreSecure() {
		t.Error("secure_cookies defaulted to false")
	}
	if cfg.HSTS {
		t.Error("HSTS defaulted to on; that locks out a deployment with no certificate yet")
	}
	if cfg.SessionLifetime() != 12*time.Hour {
		t.Errorf("session lifetime = %v", cfg.SessionLifetime())
	}
}

func TestLoadConfigRequiresADatabase(t *testing.T) {
	path := writeConfig(t, `listen_addr = "127.0.0.1:9999"`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a config with no panel_dsn was accepted")
	}
	if !strings.Contains(err.Error(), "panel_dsn") {
		t.Fatalf("error %q does not name the missing field", err)
	}
}

func TestLoadConfigRejectsAMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "yok.toml")); err == nil {
		t.Fatal("a missing config file was accepted")
	}
}

// TestSecureCookiesCanBeTurnedOffButOnlyDeliberately: the pointer is
// what separates "absent" from "false", and absent has to mean secure.
func TestSecureCookiesCanBeTurnedOffButOnlyDeliberately(t *testing.T) {
	path := writeConfig(t, `
panel_dsn = "postgres://panel@localhost/crucible"
secure_cookies = false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CookiesAreSecure() {
		t.Fatal("an explicit false was ignored")
	}
}

func TestSessionLifetimeIsBounded(t *testing.T) {
	for _, hours := range []int{0, -1, 721} {
		path := writeConfig(t, `
panel_dsn = "postgres://panel@localhost/crucible"
session_lifetime_hours = `+itoa(hours)+`
`)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("session_lifetime_hours = %d was accepted", hours)
		}
	}
}

// TestUnknownTimezoneIsAnErrorNotUTC. Falling back would put every
// timestamp in the panel hours away from the customer's clock while the
// config file said otherwise: wrong, and invisible.
func TestUnknownTimezoneIsAnErrorNotUTC(t *testing.T) {
	path := writeConfig(t, `
panel_dsn = "postgres://panel@localhost/crucible"
timezone = "Mars/Olympus"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("an unknown timezone was accepted")
	}
	if !strings.Contains(err.Error(), "Mars/Olympus") {
		t.Fatalf("error %q does not name the zone", err)
	}
}

func TestConfiguredTimezoneIsUsed(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Istanbul"); err != nil {
		t.Skip("no tzdata on this host")
	}
	path := writeConfig(t, `
panel_dsn = "postgres://panel@localhost/crucible"
timezone = "Europe/Istanbul"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := cfg.Location()
	if err != nil {
		t.Fatal(err)
	}
	if loc.String() != "Europe/Istanbul" {
		t.Fatalf("Location = %v", loc)
	}
}

// TestPlaintextDeveloperPasswordIsRefusedAtStartup. The gate refuses it
// too; the point of checking here is that the operator finds out when
// the process starts, not the first time somebody tries to change a
// guarded setting.
func TestPlaintextDeveloperPasswordIsRefusedAtStartup(t *testing.T) {
	path := writeConfig(t, `
panel_dsn = "postgres://panel@localhost/crucible"

[developer_gate]
password = "hunter2hunter2hunter2"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a plaintext developer password was accepted")
	}
	if !strings.Contains(err.Error(), "password_hash") {
		t.Fatalf("error %q does not say what to do instead", err)
	}
}

func TestMalformedDeveloperHashIsRefusedAtStartup(t *testing.T) {
	path := writeConfig(t, `
panel_dsn = "postgres://panel@localhost/crucible"

[developer_gate]
password_hash = "not-an-argon2id-hash"
`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("a malformed hash was accepted; a mistyped hash and a wrong password look identical later")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// A key that is absent and a key that is wrong are different situations,
// and only the second is a mistake somebody made.
//
// Absent means mail was never configured, which is a supported
// deployment - the panel starts and says so on one page. Malformed means
// somebody sat down to configure it and truncated a paste, and letting
// that start produces a panel that reports "this password cannot be
// decrypted" about every password it is ever given, with the cause in a
// file nobody has a reason to open again.
func TestSecretKeyIsOptionalButNeverHalfConfigured(t *testing.T) {
	const dsn = `panel_dsn = "postgres://panel_user:x@localhost:5432/analytics"` + "\n"

	valid := strings.Repeat("ab", keyHexLen/2)

	tests := []struct {
		name    string
		line    string
		wantErr bool
	}{
		{"absent", "", false},
		{"empty string", `secret_key = ""`, false},
		{"valid hex", `secret_key = "` + valid + `"`, false},
		{"valid base64", `secret_key = "` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)) + `"`, false},
		{"truncated paste", `secret_key = "` + valid[:40] + `"`, true},
		{"one character short", `secret_key = "` + valid[:len(valid)-1] + `"`, true},
		{"not an encoding", `secret_key = "bu bir anahtar değil"`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, dsn+tc.line+"\n"))
			if tc.wantErr {
				if err == nil {
					t.Fatal("the panel started with a malformed secret_key")
				}
				if !strings.Contains(err.Error(), "secret_key") {
					t.Errorf("the error does not name the setting: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			key, keyErr := cfg.Secrets()
			if tc.line == "" || strings.Contains(tc.line, `""`) {
				if !errors.Is(keyErr, sealed.ErrNoKey) {
					t.Errorf("Secrets() = %v, want ErrNoKey for an unconfigured key", keyErr)
				}
				if key.IsSet() {
					t.Error("IsSet() is true with no key configured")
				}
				return
			}
			if keyErr != nil {
				t.Fatalf("Secrets(): %v", keyErr)
			}
			// The key the panel will actually use has to work, not merely
			// parse - a config test that stopped at "no error" would pass
			// on a key that seals nothing.
			box, err := key.Seal("panel_smtp.password", "gizli")
			if err != nil {
				t.Fatal(err)
			}
			if got, err := key.Open("panel_smtp.password", box); err != nil || got != "gizli" {
				t.Errorf("the configured key does not round-trip: %q, %v", got, err)
			}
		})
	}
}

// keyHexLen is the hex length of a key, kept here rather than
// hardcoded so a change to sealed.KeySize reaches this file.
const keyHexLen = sealed.KeySize * 2
