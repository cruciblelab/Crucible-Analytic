package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/devseal"
)

// Checking a backup without restoring it.
//
// # The question this answers, and the one it does not
//
// "Is this file the backup the catalogue says it is." Not "will it
// restore", which needs a database and is the other half of F1f.
//
// The distinction matters because of when each is used. A restore into
// a side database is a deliberate exercise somebody schedules. This is
// the check a person runs on a Tuesday because they are about to do
// something irreversible, and it has to be cheap enough that running it
// is not itself a decision.
//
// # What it actually measures
//
// Every one of these has a failure it was written for:
//
//   - The size and the SHA-256 against the catalogue row. A file
//     truncated by a full disk, or corrupted by the storage under it,
//     is the ordinary way a backup stops being one - and nothing else
//     in this product would ever notice, because nothing reads a backup
//     until somebody needs it.
//
//   - Every table the manifest names has an entry, and its row count
//     matches. This is the pg_dump failure the package comment
//     describes, arrived at from the other side: a plausible file with
//     a manifest and no rows.
//
//   - Every entry is named in the manifest. The reverse direction, and
//     the one that catches a file which carries more than it admits to.
//
// # Why the row count is measured rather than trusted
//
// The manifest is part of the file. A file that says it holds 8050 rows
// and holds 8050 rows agrees with itself, which is not evidence - so
// the rows are counted out of the archive and compared with what the
// manifest claims. The two numbers come from different places, and that
// is the whole of what makes the comparison worth making.

// Verification is what a check found.
//
// Problems is a list rather than an error because a person checking a
// backup wants everything that is wrong with it, not the first thing.
// An error from Verify is different: it means the file could not be
// read far enough to have findings.
type Verification struct {
	// Path is the file that was checked, for the log line. The panel
	// never sees it - its role is not granted that column.
	Path string
	// Bytes and SHA256 are what was found on the disk, not what the
	// catalogue claimed.
	Bytes  int64
	SHA256 string
	// Secrets is whether this is the sealed configuration rather than a
	// data backup. The two are checked differently.
	Secrets bool
	// Tables is every table found, with what the manifest wanted.
	Tables []VerifiedTable
	// Problems is every disagreement, in the words the page shows.
	Problems []string
}

// VerifiedTable is one table's count, measured against the claim.
type VerifiedTable struct {
	Name string
	// Rows is what was counted in the archive.
	Rows int64
	// Wanted is what the manifest said.
	Wanted int64
}

// OK reports whether the file is what it says it is.
func (v Verification) OK() bool { return len(v.Problems) == 0 }

// RowsFound is the total counted, for the sentence the page shows.
func (v Verification) RowsFound() int64 {
	var n int64
	for _, t := range v.Tables {
		n += t.Rows
	}
	return n
}

// Verify checks one backup against its catalogue row.
//
// want carries the row: its size and checksum are what the file is
// measured against. A zero-valued want skips those two comparisons,
// which is what a caller holding a file and no catalogue has.
//
// recipient is what this deployment seals secrets backups to now, and
// it is compared with what the file was sealed to. A zero one means
// none is configured, and then nothing is compared - see
// verifySecrets.
func Verify(path string, want Backup, recipient devseal.Recipient) (Verification, error) {
	out := Verification{Path: path}

	f, err := os.Open(path)
	if err != nil {
		return out, fmt.Errorf("backup: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return out, fmt.Errorf("backup: %s: %w", path, err)
	}
	out.Bytes = info.Size()

	// Hashed in one pass with the archive read, rather than by reading
	// the file twice: the traffic tables make this the largest file on
	// the machine, and a second pass would also be a second chance for
	// the answer to be about a different file.
	sum := sha256.New()
	hashed := io.TeeReader(f, sum)
	gz, err := gzip.NewReader(hashed)
	if err != nil {
		return out, fmt.Errorf("backup: %s does not decompress, so it is not a backup "+
			"this program wrote: %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	found, manifest, secrets, err := walkArchive(gz)
	if err != nil {
		return out, fmt.Errorf("backup: reading %s: %w", path, err)
	}
	// Drained through the same tee, not straight off the file.
	//
	// tar stops at the end-of-archive marker, so anything after it in
	// the file has to be pulled through the hash explicitly or the
	// checksum is of a prefix - which would never match the catalogue,
	// and every verification on every deployment would report a good
	// backup as damaged.
	//
	// Measured, and the measurement is worth recording because it does
	// not say what it looks like it says: replacing `hashed` with `f`
	// here changes nothing today, on files of any size. Go's gzip
	// reader wraps its source in a bufio.Reader and consumes the
	// deflate trailer before reporting EOF, so by the time tar stops,
	// the tee has already seen the whole file and there is nothing left
	// for this line to read.
	//
	// So this is not a bug fix and there is no test that can tell the
	// two apart. It is written this way because the version that works
	// does so by relying on read-ahead in a package that never promised
	// any, and a checksum that is right for that reason is one that
	// stops being right without anything changing here.
	if _, err := io.Copy(io.Discard, hashed); err != nil {
		return out, fmt.Errorf("backup: reading the rest of %s: %w", path, err)
	}
	out.SHA256 = hex.EncodeToString(sum.Sum(nil))
	out.Secrets = secrets

	if want.Bytes != 0 && out.Bytes != want.Bytes {
		out.Problems = append(out.Problems, fmt.Sprintf(
			"the catalogue says %d bytes and the file is %d", want.Bytes, out.Bytes))
	}
	if want.SHA256 != "" && out.SHA256 != want.SHA256 {
		out.Problems = append(out.Problems, fmt.Sprintf(
			"the checksum does not match the catalogue (%s on disk, %s recorded)",
			out.SHA256, want.SHA256))
	}

	if secrets {
		out.Problems = append(out.Problems, verifySecrets(path, found, recipient)...)
		return out, nil
	}
	tables, problems := verifyTables(manifest, found)
	out.Tables = tables
	out.Problems = append(out.Problems, problems...)
	return out, nil
}

// walkArchive reads every entry once: the manifest into a struct, the
// data entries into row counts.
//
// Counting here rather than in a second pass because a tar is a stream
// and reading it twice means decompressing it twice. On the largest
// file this product makes, that is the difference between a check
// somebody runs and one they do not.
func walkArchive(r io.Reader) (found map[string]int64, m Manifest, secrets bool, err error) {
	found = map[string]int64{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, Manifest{}, false, err
		}
		switch {
		case h.Name == SecretsPayloadName:
			// Its presence is the whole of what can be checked without
			// the developer password. See verifySecrets.
			secrets = true
			n, err := io.Copy(io.Discard, io.LimitReader(tr, MaxSecretsBytes*2))
			if err != nil {
				return nil, Manifest{}, false, err
			}
			found[h.Name] = n
		case h.Name == ManifestName:
			body, err := io.ReadAll(io.LimitReader(tr, maxManifest))
			if err != nil {
				return nil, Manifest{}, false, err
			}
			// A secrets file's manifest is a different type in the same
			// entry name. Unmarshalling it as a data manifest leaves
			// every field zero rather than failing, which is why
			// `secrets` is decided by the payload entry above and not
			// by whether this parsed.
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, Manifest{}, false, fmt.Errorf("the manifest is not readable: %w", err)
			}
			found[h.Name] = int64(len(body))
		case isDataEntry(h.Name):
			n, err := countCopyRows(tr)
			if err != nil {
				return nil, Manifest{}, false, err
			}
			found[h.Name] = n
		default:
			// Counted so the "carries more than it admits to" check
			// below has something to see.
			n, err := io.Copy(io.Discard, tr)
			if err != nil {
				return nil, Manifest{}, false, err
			}
			found[h.Name] = n
		}
	}
	return found, m, secrets, nil
}

// verifyTables compares what the manifest claims against what is there.
func verifyTables(m Manifest, found map[string]int64) ([]VerifiedTable, []string) {
	var problems []string
	if len(m.Tables) == 0 {
		problems = append(problems, "the manifest names no tables, so this file claims to "+
			"contain nothing")
	}

	var tables []VerifiedTable
	claimed := map[string]bool{ManifestName: true}
	for _, t := range m.Tables {
		entry := "data/" + t.Name + ".copy"
		claimed[entry] = true
		rows, ok := found[entry]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"the manifest names %s and the file has no data for it", t.Name))
			continue
		}
		tables = append(tables, VerifiedTable{Name: t.Name, Rows: rows, Wanted: t.Rows})
		if rows != t.Rows {
			problems = append(problems, fmt.Sprintf(
				"%s should hold %d rows and the file holds %d", t.Name, t.Rows, rows))
		}
	}

	// The other direction. A file carrying an entry its own manifest
	// does not mention is one a restore would skip in silence.
	var extra []string
	for name := range found {
		if !claimed[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		problems = append(problems, fmt.Sprintf(
			"the file carries %s and its manifest does not mention it", name))
	}
	return tables, problems
}

// verifySecrets is what can be checked about a sealed file without the
// developer password.
//
// Which is less than for a data backup, and deliberately so: the whole
// point of internal/devseal is that this machine cannot open the file.
// Nothing here decrypts anything. What is checked is that the two
// members are present, that the recipient in the manifest is one this
// build can parse, and - through the checksum above - that the bytes
// are the bytes the catalogue recorded.
//
// The one thing that cannot be checked here is whether the payload
// authenticates, because that needs the key. `devpass -open` is where
// that answer comes from, and it comes from the person who has the
// password.
// # And the one thing about a sealed file worth saying out loud
//
// Which key it was sealed to, compared with the one this deployment
// uses now. They differ when the developer password has been changed
// since the file was taken - and the file is then perfectly intact and
// unopenable with anything anybody currently has.
//
// That is the sentence somebody needs before they need it, not after.
// It is reported as a problem because "this backup will not open with
// the password you have" is a problem, even though nothing is wrong
// with the bytes; the wording says which.
//
// Nothing is compared when no recipient is configured. That deployment
// stopped taking secrets backups, or never started, and has no current
// key to disagree with.
func verifySecrets(path string, found map[string]int64, recipient devseal.Recipient) []string {
	var problems []string
	if found[SecretsPayloadName] == 0 {
		problems = append(problems, "the sealed payload is missing or empty, so there is "+
			"nothing in this file to open")
	}
	if _, ok := found[SecretsManifestName]; !ok {
		problems = append(problems, "there is no manifest, so nothing says which key opens this")
		return problems
	}
	if !recipient.IsSet() {
		return problems
	}

	// Read a second time rather than carried out of walkArchive. The
	// manifest of a sealed file is a different type from a data
	// backup's, and threading both through one walk would put a
	// question about one kind of file into the path both take. This is
	// a few hundred bytes at the front of the archive.
	sealedTo, err := sealedRecipient(path)
	if err != nil {
		problems = append(problems, err.Error())
		return problems
	}
	if sealedTo != recipient.String() {
		problems = append(problems, "this file was sealed to a different key than the one "+
			"configured now, so the developer password in use today will not open it. "+
			"It needs the password that was in use when it was taken")
	}
	return problems
}

// sealedRecipient is the recipient line out of a sealed file's manifest.
func sealedRecipient(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("backup: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	m, err := PeekSecrets(f)
	if err != nil {
		return "", err
	}
	if _, err := devseal.ParseRecipient(m.Recipient); err != nil {
		return "", fmt.Errorf("the key this file names is not one this build can read: %w", err)
	}
	return m.Recipient, nil
}

// countCopyRows counts the rows in one COPY text entry.
//
// # Why counting newlines is exact and not an approximation
//
// PostgreSQL's COPY text format escapes every byte that could be
// mistaken for structure: a newline inside a value is written as the
// two characters `\` and `n`, a carriage return as `\r`, and a
// backslash as `\\`. So a literal newline byte in the stream is always
// a row terminator and never part of a value.
//
// That is what makes this measurement worth anything. A count that
// might be wrong for some data would be a check somebody has to
// interpret, and a check somebody interprets is one they eventually
// wave through.
func countCopyRows(r io.Reader) (int64, error) {
	var rows int64
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		rows += int64(bytes.Count(buf[:n], []byte{'\n'}))
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
	}
}

// isDataEntry reports whether a tar entry is a table's COPY data.
func isDataEntry(name string) bool {
	_, ok := tableOfEntry(name)
	return ok && strings.HasPrefix(name, "data/")
}
