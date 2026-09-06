package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/devseal"
)

// A backup file, built by hand so a test can break exactly one thing
// about it.
//
// # Why not use the writer
//
// Because every case below is a file the writer cannot produce: a
// truncated one, one whose manifest lies about a row count, one
// carrying an entry it does not admit to. Those are the files this
// check exists for, and a fixture that could only be made by the code
// under test would leave every one of them untested.
type fixture struct {
	tables map[string][]string // table -> COPY lines
	// claim overrides what the manifest says a table holds. Absent
	// means "tell the truth".
	claim map[string]int64
	// extra is an entry written into the archive and left out of the
	// manifest.
	extra map[string]string
	// omit is a table named in the manifest with no data written.
	omit map[string]bool
}

func (f fixture) write(t *testing.T, dir, name string) (path string, bytesOnDisk int64, sum string) {
	t.Helper()
	path = filepath.Join(dir, name)

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)

	m := Manifest{
		TakenAt:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Sets:          []string{SetPanel},
		BinaryVersion: "v0.0.0-test",
		SchemaVersion: 99,
	}
	for _, table := range sortedKeys(f.tables) {
		lines := f.tables[table]
		body := ""
		for _, l := range lines {
			body += l + "\n"
		}
		rows := int64(len(lines))
		if claimed, ok := f.claim[table]; ok {
			rows = claimed
		}
		m.Tables = append(m.Tables, Table{
			Name: table, Columns: []string{"a"}, Rows: rows, Bytes: int64(len(body)),
		})
		if f.omit[table] {
			continue
		}
		if err := writeEntry(tw, "data/"+table+".copy", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range sortedKeys(f.extra) {
		if err := writeEntry(tw, name, []byte(f.extra[name])); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEntry(tw, ManifestName, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw.Bytes())
	return path, int64(raw.Len()), hex.EncodeToString(h[:])
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Sorted so a fixture is the same bytes every time, which is what
	// makes a recorded checksum mean anything.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func goodFixture() fixture {
	return fixture{tables: map[string][]string{
		"panel_users":    {"1\tbir", "2\tiki", "3\tuc"},
		"panel_settings": {"a\t1"},
	}}
}

func TestAGoodBackupPasses(t *testing.T) {
	path, size, sum := goodFixture().write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.OK() {
		t.Fatalf("a file straight off the writer did not pass: %v", got.Problems)
	}
	if got.SHA256 != sum {
		t.Errorf("checksum:\n got %s\nwant %s", got.SHA256, sum)
	}
	if got.Bytes != size {
		t.Errorf("size: got %d want %d", got.Bytes, size)
	}
	if got.RowsFound() != 4 {
		t.Errorf("counted %d rows, want 4", got.RowsFound())
	}
	if len(got.Tables) != 2 {
		t.Errorf("found %d tables, want 2", len(got.Tables))
	}
}

// The failure the catalogue comparison exists for: a file the storage
// under it corrupted, or a write a full disk cut short. Nothing else in
// this product would notice, because nothing reads a backup until
// somebody needs it.
func TestAChangedByteIsCaught(t *testing.T) {
	dir := t.TempDir()
	path, size, sum := goodFixture().write(t, dir, "yedek.tar.gz")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A byte inside the deflate stream, past the gzip header, so the
	// file still looks like a backup to anything that only checks the
	// first two bytes.
	raw[len(raw)-20] ^= 0x40
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		// Corrupting deflate usually breaks the stream outright, which
		// is a perfectly good outcome: the file is refused and named.
		if !strings.Contains(err.Error(), path) {
			t.Errorf("the failure does not name the file: %v", err)
		}
		return
	}
	if got.OK() {
		t.Fatal("an edited file passed the check")
	}
	if !strings.Contains(strings.Join(got.Problems, "\n"), "checksum") {
		t.Errorf("the problems do not mention the checksum: %v", got.Problems)
	}
}

func TestATruncatedFileIsCaught(t *testing.T) {
	dir := t.TempDir()
	path, size, sum := goodFixture().write(t, dir, "yedek.tar.gz")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err == nil && got.OK() {
		t.Fatal("half a file passed the check")
	}
}

// The manifest is part of the file, so a file that agrees with itself
// is not evidence. This is what makes the comparison worth making: the
// count comes out of the archive and the claim comes out of the
// manifest, and they are two different places.
func TestAManifestThatLiesAboutRowCountsIsCaught(t *testing.T) {
	f := goodFixture()
	f.claim = map[string]int64{"panel_users": 8050}
	path, size, sum := f.write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.OK() {
		t.Fatal("a file claiming 8050 rows and holding 3 passed")
	}
	joined := strings.Join(got.Problems, "\n")
	if !strings.Contains(joined, "panel_users") || !strings.Contains(joined, "8050") {
		t.Errorf("the problem does not say which table or by how much: %v", got.Problems)
	}

	// And the measured count is reported, not only the disagreement:
	// somebody looking at this wants to know what is actually in there.
	for _, tbl := range got.Tables {
		if tbl.Name == "panel_users" && tbl.Rows != 3 {
			t.Errorf("counted %d rows in panel_users, want 3", tbl.Rows)
		}
	}
}

// The pg_dump failure this package exists because of, arrived at from
// the other side: a plausible file with a manifest and no rows in it.
func TestAManifestNamingATableWithNoDataIsCaught(t *testing.T) {
	f := goodFixture()
	f.omit = map[string]bool{"panel_users": true}
	path, size, sum := f.write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.OK() {
		t.Fatal("a file whose manifest names a table it does not carry passed")
	}
	// The sentence, not just the table name.
	//
	// A missing entry counts as zero rows, so the row-count comparison
	// reports a problem naming panel_users as well - which means an
	// assertion on the name alone passes whether the entry check runs
	// or not. Found by mutation: removing it entirely was green.
	//
	// The difference is what the person reads. "No data for it" sends
	// somebody to look at the file; "should hold 3 rows and holds 0"
	// sends them to look at the database.
	joined := strings.Join(got.Problems, "\n")
	if !strings.Contains(joined, "has no data for it") {
		t.Errorf("the problems do not say the entry is missing, only that the count is "+
			"wrong: %v", got.Problems)
	}
}

// The other direction. An entry the manifest does not mention is one a
// restore skips in silence.
func TestAnEntryTheManifestDoesNotMentionIsCaught(t *testing.T) {
	f := goodFixture()
	f.extra = map[string]string{"data/panel_sessions.copy": "1\tjeton\n"}
	path, size, sum := f.write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.OK() {
		t.Fatal("a file carrying an unlisted entry passed")
	}
	if !strings.Contains(strings.Join(got.Problems, "\n"), "panel_sessions") {
		t.Errorf("the problem does not name the entry: %v", got.Problems)
	}
}

func TestAFileWithNoManifestIsCaught(t *testing.T) {
	path, size, sum := fixture{}.write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.OK() {
		t.Fatal("a file claiming nothing passed the check")
	}
}

func TestSomethingThatIsNotABackupIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yedek.tar.gz")
	if err := os.WriteFile(path, []byte("bu bir yedek degil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, Backup{}, devseal.Recipient{}); err == nil {
		t.Fatal("a text file was accepted as a backup")
	}
	if _, err := Verify(filepath.Join(dir, "yok.tar.gz"), Backup{}, devseal.Recipient{}); err == nil {
		t.Fatal("a file that is not there was accepted")
	}
}

// A caller holding a file and no catalogue row skips the two
// comparisons that need one, and still gets everything else.
func TestAZeroCatalogueRowSkipsOnlyTheComparisons(t *testing.T) {
	path, _, _ := goodFixture().write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.OK() {
		t.Fatalf("a good file failed with no catalogue row to compare against: %v",
			got.Problems)
	}
	if got.SHA256 == "" {
		t.Error("the checksum was not computed, so nothing could be compared later")
	}
}

// The count has to be exact for data that contains newlines, or the
// check is one somebody has to interpret - and a check somebody
// interprets is one they eventually wave through.
func TestRowsAreCountedExactlyWhenValuesContainNewlines(t *testing.T) {
	f := fixture{tables: map[string][]string{
		// COPY text escapes a newline inside a value as the two
		// characters `\` and `n`, so this is one row, not two.
		"panel_settings": {`a\ni\tbir`, `b\nc\nd\te`, `c\tuc`},
	}}
	path, size, sum := f.write(t, t.TempDir(), "yedek.tar.gz")

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.OK() {
		t.Fatalf("escaped newlines were counted as row terminators: %v", got.Problems)
	}
	if got.RowsFound() != 3 {
		t.Errorf("counted %d rows, want 3", got.RowsFound())
	}
}

func TestCountCopyRowsCountsAnEmptyTableAsZero(t *testing.T) {
	n, err := countCopyRows(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("an empty entry counted as %d rows", n)
	}
}

// A backup at a realistic size still passes, and its checksum is the
// checksum of the file.
//
// # What this does and does not prove
//
// It exercises the whole read at a size where every buffer in the chain
// - gzip's bufio.Reader, tar's block reader, the 64 KiB the row counter
// uses - is filled and refilled several times. Everything above runs on
// files of a few hundred bytes, where none of that happens.
//
// What it does *not* prove is that the hash covers the tail explicitly,
// because measurement says nothing can: Go's gzip reader consumes the
// deflate trailer before reporting EOF, so the file is exhausted by the
// time tar stops however the last read is written. See the comment on
// that line in Verify. This test is honest about being a size test.
func TestALargeBackupVerifiesAndItsChecksumIsOfTheWholeFile(t *testing.T) {
	// Random bytes, hex-encoded, because the point is the size of the
	// *compressed* file. The first version of this used sequential
	// integers with a fixed suffix, which gzip took down to 52 KB - and
	// the test then failed on its own size assertion rather than on the
	// thing it is about. A red test is not evidence either.
	lines := make([]string, 5000)
	raw := make([]byte, 48)
	for i := range lines {
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		lines[i] = strconv.Itoa(i) + "\t" + hex.EncodeToString(raw)
	}
	f := fixture{tables: map[string][]string{"panel_audit_log": lines}}
	path, size, sum := f.write(t, t.TempDir(), "yedek.tar.gz")

	// The fixture is only evidence if it is actually large. A test that
	// silently shrank to nothing would go on passing.
	if size < 64*1024 {
		t.Fatalf("the fixture is %d bytes, which is too small to exercise the buffering "+
			"this test is about", size)
	}

	got, err := Verify(path, Backup{Bytes: size, SHA256: sum}, devseal.Recipient{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.SHA256 != sum {
		t.Fatalf("the checksum is of part of the file, not all of it:\n got %s\nwant %s\n"+
			"Every verification on a real deployment would report a good backup as "+
			"damaged", got.SHA256, sum)
	}
	if got.Bytes != size {
		t.Errorf("size: got %d want %d", got.Bytes, size)
	}
	if !got.OK() {
		t.Fatalf("a large good file did not pass: %v", got.Problems)
	}
	if got.RowsFound() != int64(len(lines)) {
		t.Errorf("counted %d rows, want %d", got.RowsFound(), len(lines))
	}
}
