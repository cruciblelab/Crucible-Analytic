package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devseal"
)

const testPassword = "correct horse battery staple"

// One recipient for the whole package, derived once.
//
// devseal deliberately offers no way to pick a cheaper cost: the only
// route to an identity is a password, and the cost is whatever the
// recipient names. So this pays devseal.Current once - a third of a
// second and 128 MiB - rather than per test, and the tests that open a
// file pay it again on the way back in, which is the real path and
// worth measuring.
var testRecipient = sync.OnceValue(func() devseal.Recipient {
	id, err := devseal.Generate(testPassword)
	if err != nil {
		panic("devseal.Generate: " + err.Error())
	}
	return id.Recipient()
})

// confDir builds a plausible configuration directory.
func confDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chmod(filepath.Join(dir, name), mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}
	write("collector.toml", "[privacy]\nip_hash_key = \"SIR-IP-ANAHTARI\"\n", 0o640)
	write("panel.toml", "secret_key = \"SIR-OTURUM-ANAHTARI\"\n", 0o640)
	write("upgrader.toml", "schema_admin_dsn = \"postgres://SIR-DSN\"\n", 0o640)
	write("ip_hash_key", "SIR-IP-ANAHTARI\n", 0o600)
	// Not configuration, and not collected: the pattern is *.toml and
	// this is what proves the pattern is doing something.
	write("README", "notes\n", 0o644)
	return dir
}

func writeSecrets(t *testing.T, conf, dst string, r devseal.Recipient) Result {
	t.Helper()
	w := SecretsWriter{
		ConfDir:       conf,
		Dir:           dst,
		Recipient:     r,
		BinaryVersion: "v0.0.0-test",
		SchemaVersion: 13,
	}
	res, err := w.Write(context.Background(), "sirlar-test.tar.gz")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	return res
}

func TestASecretsBackupRoundTrips(t *testing.T) {
	conf := confDir(t)
	dst := t.TempDir()
	res := writeSecrets(t, conf, dst, testRecipient())

	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	secrets, err := OpenSecrets(f, testPassword)
	if err != nil {
		t.Fatalf("open secrets: %v", err)
	}

	want := map[string]string{
		"collector.toml": "[privacy]\nip_hash_key = \"SIR-IP-ANAHTARI\"\n",
		"panel.toml":     "secret_key = \"SIR-OTURUM-ANAHTARI\"\n",
		"upgrader.toml":  "schema_admin_dsn = \"postgres://SIR-DSN\"\n",
		"ip_hash_key":    "SIR-IP-ANAHTARI\n",
	}
	got := map[string]string{}
	for _, file := range secrets.Files {
		got[file.Name] = string(file.Bytes)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d", len(got), slices.Sorted(maps.Keys(got)), len(want))
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s:\n got %q\nwant %q", name, got[name], body)
		}
	}
	if _, ok := got["README"]; ok {
		t.Error("a file that is not configuration was collected")
	}

	// The modes travel, because a restore has to put them back:
	// upgrader.toml written at 0644 hands the schema_admin credential
	// to every account on the machine.
	for _, file := range secrets.Files {
		want := os.FileMode(0o640)
		if file.Name == "ip_hash_key" {
			want = 0o600
		}
		if file.Mode != want {
			t.Errorf("%s came back at %04o, want %04o", file.Name, file.Mode, want)
		}
	}
}

// The decision this file turns on: the plaintext half says nothing
// about the contents.
//
// A manifest listing each file with its SHA-256 would be a verifier for
// the plaintext. Config files are boilerplate around a few secrets, so
// somebody holding the backup could guess a password field and check
// the guess against the hash for the cost of one SHA-256 - never paying
// the argon2id cost that is the entire defence. Sizes leak less and
// still leak.
//
// Checked by naming the whole key set rather than by looking for the
// fields that are wrong today, so that a field added later has to be
// added here too, in front of somebody who has to think about it.
func TestTheOuterManifestSaysNothingAboutTheContents(t *testing.T) {
	conf := confDir(t)
	res := writeSecrets(t, conf, t.TempDir(), testRecipient())

	raw := readMember(t, res.Path, SecretsManifestName)
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"alici", "alindi", "gecici_anahtar", "kumeler", "sema_surumu", "surum"}
	got := slices.Sorted(maps.Keys(fields))
	if !slices.Equal(got, want) {
		t.Fatalf("the plaintext manifest carries %v, want exactly %v.\n"+
			"Anything about the contents - a name, a size, a hash - belongs inside "+
			"the sealed payload; see the comment on this test", got, want)
	}
}

// And the whole file, not just the manifest, must not carry a secret in
// the clear. A member added to the outer archive by mistake, or a
// payload that failed to seal and fell back to plaintext, would show up
// here and nowhere else.
func TestNothingInTheFileIsReadableWithoutThePassword(t *testing.T) {
	conf := confDir(t)
	res := writeSecrets(t, conf, t.TempDir(), testRecipient())

	onDisk, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Compressed, so the plaintext would not appear literally in the
	// bytes even if it were there. Decompressed and searched.
	plain := decompress(t, onDisk)
	for _, secret := range []string{"SIR-IP-ANAHTARI", "SIR-OTURUM-ANAHTARI", "SIR-DSN"} {
		if bytes.Contains(plain, []byte(secret)) {
			t.Errorf("%q appears in the file without being sealed", secret)
		}
	}
	// The filenames are not in the clear either. They say which
	// services this deployment runs, which is not a secret - but it is
	// free not to say, and the listing is inside the payload where it
	// is worth having.
	for _, name := range []string{"collector.toml", "ip_hash_key"} {
		if bytes.Contains(plain, []byte(name)) {
			t.Errorf("the filename %q appears outside the sealed payload", name)
		}
	}
}

func TestOpenSecretsRefusesTheWrongPassword(t *testing.T) {
	conf := confDir(t)
	res := writeSecrets(t, conf, t.TempDir(), testRecipient())

	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := OpenSecrets(f, testPassword+"!"); !errors.Is(err, devseal.ErrWrongPassword) {
		t.Fatalf("got %v, want ErrWrongPassword", err)
	}
}

// PeekSecrets is what somebody holding an unlabelled file runs first,
// and it must not need a password to answer.
func TestPeekSaysWhatTheFileIsWithoutAPassword(t *testing.T) {
	conf := confDir(t)
	r := testRecipient()
	res := writeSecrets(t, conf, t.TempDir(), r)

	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	head, err := PeekSecrets(f)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if head.Recipient != r.String() {
		t.Errorf("recipient:\n got %s\nwant %s", head.Recipient, r)
	}
	if head.BinaryVersion != "v0.0.0-test" || head.SchemaVersion != 13 {
		t.Errorf("versions: %s / %d", head.BinaryVersion, head.SchemaVersion)
	}
	if !slices.Equal(head.Sets, []string{SetSirlar}) {
		t.Errorf("sets: %v", head.Sets)
	}
	if head.TakenAt.IsZero() {
		t.Error("no date")
	}
	if head.Ephemeral == "" {
		t.Error("no ephemeral key, so nothing could ever open this")
	}
}

// The header binds the metadata to the ciphertext. Editing the date in
// a backup - to make a file look newer than it is, which is exactly
// what somebody covering their tracks would want - must stop it
// opening rather than succeed quietly.
func TestEditingTheManifestStopsTheFileOpening(t *testing.T) {
	conf := confDir(t)
	res := writeSecrets(t, conf, t.TempDir(), testRecipient())

	original := readMember(t, res.Path, SecretsManifestName)
	var m SecretsManifest
	if err := json.Unmarshal(original, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m.BinaryVersion = "v9.9.9"
	edited, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	tampered := replaceMember(t, res.Path, SecretsManifestName, edited)
	if _, err := OpenSecrets(bytes.NewReader(tampered), testPassword); err == nil {
		t.Fatal("a file whose manifest was edited still opened")
	}
}

func TestKindOfKeepsTheTwoArtifactsApart(t *testing.T) {
	cases := []struct {
		name string
		sets []string
		want Kind
		fail bool
	}{
		{name: "the traffic", sets: []string{SetAnalitik}, want: KindData},
		{name: "the panel", sets: []string{SetPanel}, want: KindData},
		{name: "both table sets", sets: []string{SetAnalitik, SetPanel}, want: KindData},
		{name: "the configuration", sets: []string{SetSirlar}, want: KindSecrets},
		{name: "configuration and traffic", sets: []string{SetSirlar, SetAnalitik}, fail: true},
		{name: "configuration and panel", sets: []string{SetPanel, SetSirlar}, fail: true},
		{name: "everything", sets: SetNames(), fail: true},
		{name: "nothing", sets: nil, fail: true},
		{name: "a name this build does not know", sets: []string{"analytics"}, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KindOf(tc.sets)
			if tc.fail {
				if err == nil {
					t.Fatalf("got %q, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The refusal has to be there twice: KindOf routes, and TablesFor is
// what would silently produce an empty dump if anything ever reached it
// with the secrets set.
func TestTablesForRefusesTheSecretsSet(t *testing.T) {
	tables, err := TablesFor([]string{SetSirlar})
	if err == nil {
		t.Fatalf("got %v, want a refusal", tables)
	}
	if len(tables) != 0 {
		t.Errorf("a refusal returned tables: %v", tables)
	}
}

// Set.Secrets and "has no tables" must never disagree, because the
// whole point of the field is that an edit which empties a set's table
// list does not turn it into a secrets backup.
func TestExactlyOneSetIsSecrets(t *testing.T) {
	var secrets []string
	for _, s := range Sets {
		if s.Secrets {
			secrets = append(secrets, s.Name)
			if len(s.Tables) > 0 {
				t.Errorf("%s is marked as the configuration and also names tables: %v",
					s.Name, s.Tables)
			}
			continue
		}
		if len(s.Tables) == 0 {
			t.Errorf("%s is a table set with no tables. Either give it tables or mark "+
				"it Secrets; a set that resolves to nothing produces a backup that "+
				"reports success and contains nothing", s.Name)
		}
	}
	if !slices.Equal(secrets, []string{SetSirlar}) {
		t.Errorf("sets marked Secrets: %v, want exactly [%s]", secrets, SetSirlar)
	}
}

func TestCollectSecretsSkipsWhatItMustNotRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "panel.toml"), []byte("x=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink would otherwise be read *through*, and whatever it
	// pointed at would land inside a file the customer may ask for.
	outside := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(outside, []byte("root:$6$SIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sneaky.toml")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "conf.d.toml"), 0o700); err != nil {
		t.Fatal(err)
	}

	files, index, err := CollectSecrets(dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != 1 || files[0].Name != "panel.toml" {
		t.Fatalf("collected %v, want only panel.toml", names(files))
	}
	for _, f := range files {
		if bytes.Contains(f.Bytes, []byte("SIR")) {
			t.Error("a symlink was followed out of the configuration directory")
		}
	}
	skipped := map[string]string{}
	for _, s := range index.Skipped {
		skipped[s.Name] = s.Reason
	}
	for _, name := range []string{"sneaky.toml", "conf.d.toml"} {
		if _, ok := skipped[name]; !ok {
			t.Errorf("%s was neither collected nor recorded as skipped; a file that "+
				"vanishes silently is the one nobody looks for at restore time", name)
		}
	}
}

func TestCollectSecretsRefusesSomethingTooLarge(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "panel.toml"),
		bytes.Repeat([]byte("x"), MaxSecretFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := CollectSecrets(dir)
	if err == nil {
		t.Fatal("a file over the limit was collected")
	}
	if !strings.Contains(err.Error(), "panel.toml") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

func TestCollectSecretsRefusesADirectoryWithNothingInIt(t *testing.T) {
	if _, _, err := CollectSecrets(t.TempDir()); err == nil {
		t.Fatal("an empty directory produced a secrets backup")
	}
	if _, _, err := CollectSecrets(""); err == nil {
		t.Fatal("an unset directory produced a secrets backup")
	}
}

func TestASecretsBackupNeedsARecipient(t *testing.T) {
	w := SecretsWriter{ConfDir: confDir(t), Dir: t.TempDir()}
	if _, err := w.Write(context.Background(), "sirlar-test.tar.gz"); !errors.Is(err, ErrNoRecipient) {
		t.Fatalf("got %v, want ErrNoRecipient", err)
	}
}

// The two files must not be confusable by anybody sorting a directory:
// different contents, different protections, different restore.
func TestTheTwoBackupsAreNamedDifferently(t *testing.T) {
	r := Runner{}
	data, secrets := r.fileName(), r.secretsFileName()
	if data == secrets {
		t.Fatal("both backups take the same name")
	}
	if strings.HasPrefix(secrets, "yedek-") {
		t.Errorf("a secrets backup is named like a data backup: %s", secrets)
	}
	if !strings.HasSuffix(secrets, ".tar.gz") {
		t.Errorf("%s does not say what kind of file it is", secrets)
	}
}

// The two-way mirror: every file install.sh writes into the
// configuration directory has to be one a secrets backup collects.
//
// Read from the script rather than listed here, because the failure
// this guards against is somebody adding a sixth config file to the
// installer and nobody noticing that backups stopped being complete.
// That failure is silent for exactly as long as it takes to need one.
func TestEveryFileInstallWritesIsCollected(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "release", "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	re := regexp.MustCompile(`\$\{CONF_DIR\}/([A-Za-z0-9][A-Za-z0-9._-]*)`)
	var found []string
	for _, m := range re.FindAllSubmatch(body, -1) {
		name := string(m[1])
		if !slices.Contains(found, name) {
			found = append(found, name)
		}
	}
	// A check that found nothing is not a passing check. install.sh
	// names at least the four service files, upgrader.toml and
	// ip_hash_key.
	if len(found) < 6 {
		t.Fatalf("only found %v in install.sh; the pattern that reads it has gone "+
			"stale and this test is no longer checking anything", found)
	}
	for _, name := range found {
		if !collected(t, name) {
			t.Errorf("install.sh writes %s into the configuration directory and a "+
				"secrets backup does not collect it. Either it matches %q or it "+
				"belongs in SecretsAlso", name, SecretsPattern)
		}
	}
}

func collected(t *testing.T, name string) bool {
	t.Helper()
	if slices.Contains(SecretsAlso, name) {
		return true
	}
	ok, err := filepath.Match(SecretsPattern, name)
	if err != nil {
		t.Fatalf("%q: %v", SecretsPattern, err)
	}
	return ok
}

func names(files []SecretFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}

// readMember pulls one entry out of a written backup.
func readMember(t *testing.T, path, want string) []byte {
	t.Helper()
	for name, body := range members(t, path) {
		if name == want {
			return body
		}
	}
	t.Fatalf("%s has no %s", path, want)
	return nil
}

// replaceMember rebuilds an archive with one member's bytes changed, so
// a test can edit a file the way somebody with a shell would.
func replaceMember(t *testing.T, path, target string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for name, member := range members(t, path) {
		if name == target {
			member = body
		}
		if err := writeEntry(tw, name, member); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// members reads every entry, in order.
func members(t *testing.T, path string) iter.Seq2[string, []byte] {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	type entry struct {
		name string
		body []byte
	}
	var entries []entry
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		entries = append(entries, entry{hdr.Name, body})
	}
	return func(yield func(string, []byte) bool) {
		for _, e := range entries {
			if !yield(e.name, e.body) {
				return
			}
		}
	}
}

// decompress is the file as it is on disk, minus the gzip layer, so a
// search for a secret is a search of what an archive tool would show.
func decompress(t *testing.T, raw []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}
