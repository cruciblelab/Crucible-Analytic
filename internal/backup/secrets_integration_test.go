//go:build integration

// The secrets backup, from the row somebody writes to the files
// somebody gets back.
//
// The unit tests in secrets_test.go run the writer and the reader
// against a temporary directory, which proves the format and the
// encryption. What they cannot prove is the thing this project has got
// wrong twice: that the queue and the runner are connected. So this
// starts where a person starts - a request written by the panel's role,
// naming the configuration set - and never calls SecretsWriter directly.
//
// *Her halkası test edilmiş bir zincir, test edilmiş bir zincir
// değildir.*

package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/devseal"
)

const secretsPassword = "correct horse battery staple"

// secretsConfDir is a configuration directory with something worth
// protecting in it.
func secretsConfDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"collector.toml": "[privacy]\nip_hash_key = \"GIZLI-IP-ANAHTARI\"\n",
		"panel.toml":     "secret_key = \"GIZLI-OTURUM\"\n",
		"upgrader.toml":  "schema_admin_dsn = \"postgres://schema_admin:GIZLI-DSN@localhost/x\"\n",
		"ip_hash_key":    "GIZLI-IP-ANAHTARI\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func secretsRecipient(t *testing.T) devseal.Recipient {
	t.Helper()
	id, err := devseal.Generate(secretsPassword)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return id.Recipient()
}

// TestAskingForTheConfigurationProducesASealedFileAndACatalogueRow.
func TestAskingForTheConfigurationProducesASealedFileAndACatalogueRow(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	conf := secretsConfDir(t)
	dir := t.TempDir()
	recipient := secretsRecipient(t)

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetSirlar}); err != nil {
		t.Fatal(err)
	}

	r := backup.Runner{
		Pool: answers, Dir: dir, ConfDir: conf, Recipient: recipient,
		Name: "test-upgrader", BinaryVersion: "v0.0.0-test", SchemaVersion: 99,
	}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("the runner did not carry out the request: %v", err)
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateSucceeded {
		t.Fatalf("the request is in state %q: %s", latest.State, latest.ErrorChain)
	}

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the catalogue has %d rows after one secrets backup", len(rows))
	}
	row := rows[0]

	// The catalogue says which of the two artifacts this is. Without
	// it, the page shows a file and the customer cannot tell a data
	// backup from a configuration one - which are restored differently
	// and protect different things.
	if len(row.Sets) != 1 || row.Sets[0] != backup.SetSirlar {
		t.Errorf("the catalogue row names %v, want [%s]", row.Sets, backup.SetSirlar)
	}
	if !strings.Contains(filepath.Base(row.Path), "sirlar-") {
		t.Errorf("the file is named %s; a secrets backup must not be confusable with a "+
			"data backup in a directory listing", filepath.Base(row.Path))
	}
	info, err := os.Stat(row.Path)
	if err != nil {
		t.Fatalf("the catalogue names %s and it is not there: %v", row.Path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the file is %04o; a sealed file is still not one to leave readable", perm)
	}
	if info.Size() != row.Bytes {
		t.Errorf("the catalogue says %d bytes and the file is %d", row.Bytes, info.Size())
	}

	// And the point of all of it: the password gets the configuration
	// back, and nothing else does.
	f, err := os.Open(row.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	secrets, err := backup.OpenSecrets(f, secretsPassword)
	if err != nil {
		t.Fatalf("the file the runner wrote does not open with the password it was "+
			"sealed to: %v", err)
	}
	got := map[string]string{}
	for _, file := range secrets.Files {
		got[file.Name] = string(file.Bytes)
	}
	for _, name := range []string{"collector.toml", "panel.toml", "upgrader.toml", "ip_hash_key"} {
		if got[name] == "" {
			t.Errorf("%s is not in the backup; the files were %v", name, keysOf(got))
		}
	}
	if !strings.Contains(got["collector.toml"], "GIZLI-IP-ANAHTARI") {
		t.Error("the key that makes stored addresses pseudonyms did not come back")
	}
}

// The refusal that keeps the two artifacts apart, measured on the
// answering side rather than the asking one.
//
// The panel checks it too, and that check is the one a person sees. This
// is the one that holds when the panel is not the thing writing the
// row - a compromised panel, a row written by hand, a future caller.
func TestTheRunnerRefusesARequestNamingBothKinds(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	// Inserted with SQL rather than through Ask, deliberately.
	//
	// Ask refuses this, which is the first of the two checks and the
	// one a person meets. What is being measured here is the second:
	// that the upgrader refuses it too, when the row did not come from
	// a panel that asked nicely. A compromised panel holds panel_user
	// and can INSERT - the policy says so - so this is the row it would
	// write, written the way it would write it.
	if _, err := asks.Exec(ctx, `
		INSERT INTO panel_backup_requests (actor_kind, actor_label, sets)
		VALUES ('user', 'test', $1)`,
		[]string{backup.SetPanel, backup.SetSirlar}); err != nil {
		t.Fatalf("writing the row this test needs: %v", err)
	}

	r := backup.Runner{
		Pool: answers, Dir: t.TempDir(), ConfDir: secretsConfDir(t),
		Recipient: secretsRecipient(t), Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99,
	}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("the runner carried out a request naming the configuration and the " +
			"data together")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateFailed {
		t.Fatalf("the request is in state %q, want failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "pseudonymisation") {
		t.Errorf("the row says %q, which does not tell the person at the page why",
			latest.ErrorChain)
	}

	// And nothing was written.
	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused request produced %d catalogue rows", len(rows))
	}
}

// A deployment with a backup directory and no recipient takes data
// backups and cannot take secrets backups. The request has to fail with
// that sentence on the row, not wait for an upgrader that will never be
// able to serve it.
func TestWithoutARecipientTheRequestFailsWithTheReason(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetSirlar}); err != nil {
		t.Fatal(err)
	}

	r := backup.Runner{
		Pool: answers, Dir: t.TempDir(), ConfDir: secretsConfDir(t),
		Name: "test-upgrader", BinaryVersion: "v0.0.0-test", SchemaVersion: 99,
	}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("a secrets backup was taken with no recipient, so nobody could open it")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateFailed {
		t.Fatalf("the request is in state %q, want failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "devpass -recipient") {
		t.Errorf("the row says %q and does not say what to do about it", latest.ErrorChain)
	}
}

// The two backups in one directory, which is how they will actually sit
// on a customer's disk.
func TestBothKindsCanLiveInOneDirectory(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	r := backup.Runner{
		Pool: answers, Dir: dir, ConfDir: secretsConfDir(t),
		Recipient: secretsRecipient(t), Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99,
	}
	for _, sets := range [][]string{{backup.SetPanel}, {backup.SetSirlar}} {
		if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
			sets); err != nil {
			t.Fatalf("%v: %v", sets, err)
		}
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatalf("%v: %v", sets, err)
		}
	}

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("two backups produced %d catalogue rows", len(rows))
	}
	if rows[0].Path == rows[1].Path {
		t.Fatal("both backups landed on one file")
	}

	// The data backup must not open as a secrets backup, and the
	// secrets backup must not be readable as a data one. Both are the
	// same statement from opposite sides: two artifacts, never one.
	for _, row := range rows {
		f, err := os.Open(row.Path)
		if err != nil {
			t.Fatal(err)
		}
		_, peekErr := backup.PeekSecrets(f)
		_ = f.Close()

		isSecrets := row.Sets[0] == backup.SetSirlar
		if isSecrets && peekErr != nil {
			t.Errorf("%s is the secrets backup and does not read as one: %v",
				filepath.Base(row.Path), peekErr)
		}
		if !isSecrets && peekErr == nil {
			t.Errorf("%s is the data backup and reads as a secrets file",
				filepath.Base(row.Path))
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
