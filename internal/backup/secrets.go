package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/devseal"
	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
)

// The second artifact: the configuration, encrypted to the developer
// password.
//
// # Why this is a separate file and not another set of tables
//
// internal/backup/schema.sql says it: addresses in traffic_snapshots
// are pseudonymous only because `ip_hash_key` lives somewhere else, in
// collector.toml. One file holding both the data and the key would undo
// the pseudonymisation for whoever has it. So there are two artifacts,
// and neither one alone lets its holder re-identify anybody.
//
// That is the whole reason a data backup and a secrets backup can never
// be the same file, and it is enforced rather than intended: KindOf
// refuses a request that names the secrets set alongside a table set,
// and it is called on both sides - by the panel before the row is
// written, and by the upgrader before a byte is read.
//
// # And why "separate file" was not enough on its own
//
// Written plainly, the two files would sit in one directory, owned by
// one account, at one mode. Whoever can read either can read both, and
// the split would be a fact about filenames.
//
// So this one is encrypted, and to something that is not on the
// machine: the developer password. See internal/devseal for the
// construction and for the property it buys - the machine can produce a
// secrets backup and cannot read one.
//
// # What the plaintext half is allowed to say
//
// Almost nothing, and this is the subtlest decision in the file.
//
// The obvious manifest would list each file with its size and its
// SHA-256, the way the data backup's manifest lists tables with row
// counts. That would be a disaster here. A config file is mostly
// boilerplate around a few secrets, so a hash of the plaintext is a
// verifier: somebody holding the backup could guess a password field
// and check the guess against the hash, at the cost of one SHA-256 -
// never paying the argon2id cost that is the entire defence. Sizes leak
// less and still leak: the length of a password is a real narrowing.
//
// So the outer manifest carries only what a person needs in order to
// know what the file is and how to open it - when, from which build,
// and which recipient. Everything about the contents is inside the
// sealed payload, where it is worth having and costs nothing.

// SetSirlar is the configuration files, as a set somebody can ask for.
const SetSirlar = "sirlar"

// SecretsPattern is what a secrets backup collects from the
// configuration directory.
//
// A pattern rather than a list of names, deliberately. The list of
// config files is not fixed - a deployment may not run all four
// services, and a later version may add a fifth - and a backup that
// silently omitted a file nobody had added to a constant would be
// discovered at the only moment it matters. The directory is the
// authority on what configuration this machine has.
//
// TestEverySecretInstallWritesIsCollected reads release/install.sh and
// checks that every file it writes into the configuration directory is
// picked up by this pattern or named below, so "the pattern is right"
// is measured against the script that creates them rather than
// asserted.
const SecretsPattern = "*.toml"

// SecretsAlso are files collected by name because they do not match the
// pattern.
//
// ip_hash_key is the only one, and leaving it out would be a trap
// rather than an omission. The file is a copy of a value that also
// lives inside collector.toml and beacon.toml; install.sh reads it so
// that a re-run finds the key it already generated instead of rotating
// it. Restore the two configs without this file and the next install
// run draws a new key - which orphans every pseudonym ever stored, with
// no error and no way back.
var SecretsAlso = []string{"ip_hash_key"}

// Caps on what may be collected.
//
// A configuration directory is kilobytes. These bounds exist because
// nothing stops somebody putting a database dump, a core file or a log
// in there, and a "secrets backup" that quietly grew to a gigabyte
// would be held in memory, encrypted as one AEAD message, and written
// to the disk this feature exists to avoid filling.
//
// Exceeding either is a refusal with the file named, not a truncation.
// A backup that silently left something out is the failure this whole
// package is written against.
const (
	MaxSecretFileBytes = 1 << 20
	MaxSecretsBytes    = 8 << 20
)

// Names inside a secrets file.
const (
	// SecretsManifestName is the plaintext member: what this is, and
	// which key opens it.
	SecretsManifestName = "manifest.json"
	// SecretsPayloadName is the sealed member.
	SecretsPayloadName = "sirlar.enc"
	// SecretsIndexName is the listing, inside the sealed member.
	SecretsIndexName = "icerik.json"
	// SecretsFileDir is the directory the files sit under, inside the
	// sealed member.
	SecretsFileDir = "dosyalar/"
)

// ErrNoRecipient means this deployment cannot take a secrets backup
// because no recipient is configured.
//
// Its own sentinel for the reason ErrNotConfigured has one: a queued
// request must fail with this sentence on the row, because the page is
// where somebody is waiting and "nobody could open this file" is the
// answer they need.
var ErrNoRecipient = errors.New("backup: no developer recipient is configured; " +
	"generate one with `devpass -recipient` and put it in upgrader.toml's [backup] section")

// ErrMixedRequest means one request named the configuration and the
// data together.
//
// A sentinel rather than a sentence, because the panel says something
// different for it than for every other refusal here: the person who
// ticked both boxes did a reasonable thing, and what they need is the
// reason it is two operations rather than one.
var ErrMixedRequest = errors.New("backup: the configuration and the data cannot share a file")

// Kind is which of the two artifacts a request is for.
type Kind string

const (
	// KindData is a dump of tables.
	KindData Kind = "veri"
	// KindSecrets is the configuration, sealed.
	KindSecrets Kind = "sirlar"
)

// KindOf says which artifact a request's sets describe, and refuses a
// request that describes both.
//
// This is where "two files, never one" is enforced. Not by convention
// and not by the caller remembering: the sets go through here on the
// asking side and again on the answering side, and a mix is an error
// with a sentence rather than a file with everything in it.
//
// Refused rather than split into two files, because a request is one
// row with one outcome. Two files from one row would leave the
// catalogue, the page and the operation log each describing half of
// what happened.
func KindOf(sets []string) (Kind, error) {
	if len(sets) == 0 {
		return "", fmt.Errorf("backup: no set was named; there would be nothing in the file")
	}
	var tables, secrets []string
	for _, name := range sets {
		s, err := SetByName(name)
		if err != nil {
			return "", err
		}
		if s.Secrets {
			secrets = append(secrets, s.Name)
			continue
		}
		tables = append(tables, s.Name)
	}
	if len(secrets) > 0 && len(tables) > 0 {
		return "", fmt.Errorf("%w: %q cannot be taken in the same file as %s. The "+
			"configuration holds ip_hash_key, and a file holding both it and the traffic "+
			"would undo the pseudonymisation for anybody who has it. Ask for them separately",
			ErrMixedRequest, strings.Join(secrets, ", "), strings.Join(tables, ", "))
	}
	if len(secrets) > 0 {
		return KindSecrets, nil
	}
	return KindData, nil
}

// SecretsManifest is the plaintext half of a secrets file.
//
// Everything in it is public. See the note above on what is
// deliberately absent.
type SecretsManifest struct {
	// TakenAt is when the copy was made, from the database's clock, for
	// the reason Manifest.TakenAt gives.
	TakenAt time.Time `json:"alindi"`
	// Sets is what was asked for. Always the secrets set alone.
	Sets []string `json:"kumeler"`
	// BinaryVersion and SchemaVersion are what produced it. The schema
	// version is here for the same reason it is in a data backup: a
	// restore has to know what it is looking at.
	BinaryVersion string `json:"surum"`
	SchemaVersion int    `json:"sema_surumu"`

	// Recipient is the public key this was sealed to, in full.
	//
	// Written into the file rather than left in the config, because the
	// config is what this file is a backup of. A restore happens when
	// that directory is gone, so everything needed to derive the
	// opening key - the salt, the argon2id cost, the public key to
	// check the password against - has to travel with the bytes.
	Recipient string `json:"alici"`
	// Ephemeral is the public half of the throwaway key pair the seal
	// was made with, in hex.
	//
	// Not authenticated by the header, and it does not need to be: it
	// goes into the key derivation on both sides, so a changed value
	// produces a key that does not open the box.
	Ephemeral string `json:"gecici_anahtar"`
}

// SecretsIndex is the listing inside the sealed payload.
type SecretsIndex struct {
	Files   []SecretsEntry `json:"dosyalar"`
	Skipped []SecretsSkip  `json:"atlananlar"`
}

// SecretsEntry is one file that was collected.
type SecretsEntry struct {
	Name   string `json:"ad"`
	Bytes  int64  `json:"bayt"`
	Mode   string `json:"kip"`
	SHA256 string `json:"sha256"`
}

// SecretsSkip is one file that was not, and why.
//
// Recorded rather than dropped. A secrets backup missing a file is
// still worth having, and the person restoring it needs to know which
// one and for what reason - "permission denied" and "larger than the
// limit" lead to different next steps, and both lead somewhere.
type SecretsSkip struct {
	Name   string `json:"ad"`
	Reason string `json:"sebep"`
}

// SecretFile is one collected file, with its contents.
type SecretFile struct {
	Name  string
	Mode  fs.FileMode
	Bytes []byte
}

// Secrets is an opened secrets backup.
type Secrets struct {
	Manifest SecretsManifest
	Index    SecretsIndex
	Files    []SecretFile
}

// SecretsWriter produces a secrets file.
type SecretsWriter struct {
	// Pool supplies the clock, and may be nil in a test. See now.
	Pool *pgxpool.Pool
	// ConfDir is the directory the configuration lives in.
	//
	// Derived by the upgrader from the path of its own config file, and
	// never configured separately. A [secrets] dir setting would be one
	// more thing to point at the wrong place, and a directory named in
	// a request would be a directory a compromised panel could choose -
	// the same reason panel_backup_requests carries no path.
	ConfDir string
	// Dir is where the file goes, the same directory data backups use.
	Dir string
	// Recipient is who can open it. Unset means this deployment cannot
	// take a secrets backup.
	Recipient devseal.Recipient

	BinaryVersion string
	SchemaVersion int
}

// Write collects the configuration, seals it and writes the file.
func (w SecretsWriter) Write(ctx context.Context, name string) (Result, error) {
	if !w.Recipient.IsSet() {
		return Result{}, ErrNoRecipient
	}
	files, index, err := CollectSecrets(w.ConfDir)
	if err != nil {
		return Result{}, err
	}

	takenAt, err := w.now(ctx)
	if err != nil {
		return Result{}, err
	}

	manifest := SecretsManifest{
		TakenAt:       takenAt,
		Sets:          []string{SetSirlar},
		BinaryVersion: w.BinaryVersion,
		SchemaVersion: w.SchemaVersion,
		Recipient:     w.Recipient.String(),
	}

	payload, err := sealSecrets(w.Recipient, manifest, index, files)
	if err != nil {
		return Result{}, err
	}
	manifest.Ephemeral = payload.ephemeral

	out, err := container(w.Dir, name, func(tw *tar.Writer) error {
		body, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("backup: writing the manifest: %w", err)
		}
		// The manifest first here, unlike a data backup, because
		// nothing in it depends on what comes after: it is known before
		// the payload is written and a reader that only wants to know
		// what the file is should not have to read past it.
		if err := writeEntry(tw, SecretsManifestName, body); err != nil {
			return err
		}
		return writeEntry(tw, SecretsPayloadName, []byte(payload.box))
	})
	if err != nil {
		return Result{}, err
	}
	// Result.Manifest stays the zero value: that type is the data
	// backup's account of itself and this file does not have one. What
	// the catalogue needs is these four, and the set name is what says
	// which of the two artifacts a row describes.
	out.TakenAt = manifest.TakenAt
	out.Sets = manifest.Sets
	out.BinaryVersion = manifest.BinaryVersion
	out.SchemaVersion = manifest.SchemaVersion
	return out, nil
}

// now is the database's clock, for the reason Manifest.TakenAt gives:
// every other timestamp in this product comes from there, and one that
// did not would be comparable with none of them.
//
// Nil falls back to this machine's clock, which is what a test that
// writes a file without a database uses. Not a production path: the
// request that gets here arrived through a queue in that database.
func (w SecretsWriter) now(ctx context.Context) (time.Time, error) {
	if w.Pool == nil {
		return time.Now().UTC(), nil
	}
	return databaseNow(ctx, w.Pool)
}

// sealed is what sealSecrets produced.
type sealedPayload struct {
	ephemeral string
	box       string
}

// sealSecrets builds the inner archive and encrypts it.
func sealSecrets(to devseal.Recipient, m SecretsManifest, index SecretsIndex,
	files []SecretFile) (sealedPayload, error) {

	var inner bytes.Buffer
	tw := tar.NewWriter(&inner)

	body, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return sealedPayload{}, fmt.Errorf("backup: writing the listing: %w", err)
	}
	if err := writeEntry(tw, SecretsIndexName, body); err != nil {
		return sealedPayload{}, err
	}
	for _, f := range files {
		if err := writeSecretEntry(tw, SecretsFileDir+f.Name, f.Mode, f.Bytes); err != nil {
			return sealedPayload{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return sealedPayload{}, fmt.Errorf("backup: closing the inner archive: %w", err)
	}

	// Not compressed before sealing. The saving on a few kilobytes of
	// text is not worth a second code path, and the length of a
	// ciphertext that compressed its plaintext says something about the
	// plaintext - a small thing here, and free to not say at all.
	ephemeral, box, err := to.Seal(secretsHeader(m), inner.Bytes())
	if err != nil {
		return sealedPayload{}, fmt.Errorf("backup: sealing the configuration: %w", err)
	}
	return sealedPayload{ephemeral: ephemeral, box: box}, nil
}

// secretsHeader is what the payload is authenticated against.
//
// # Why this is built by hand rather than by marshalling the manifest
//
// Because both sides have to produce the same bytes, years apart, and
// JSON round-tripping does not promise that: a field added by a later
// version is dropped when an older one unmarshals, the re-marshalled
// form differs, and the file stops opening for a reason nobody can see.
//
// So it is an explicit string, in a fixed order, with a domain line at
// the top. What is in it is what an attacker could otherwise swap
// without the ciphertext noticing: the date, the sets, the versions and
// the recipient. The ephemeral key and the salt are not in it because
// they are already bound - both go into the key derivation, so altering
// either produces a key that does not open the box.
func secretsHeader(m SecretsManifest) string {
	return strings.Join([]string{
		"crucible-analytic/secrets-backup/v1",
		m.TakenAt.UTC().Format(time.RFC3339Nano),
		strings.Join(m.Sets, ","),
		m.BinaryVersion,
		strconv.Itoa(m.SchemaVersion),
		m.Recipient,
	}, "\n")
}

// CollectSecrets reads the configuration directory.
//
// Returns what it could read and a listing that also names what it
// could not, so the caller writes both into the file.
func CollectSecrets(dir string) ([]SecretFile, SecretsIndex, error) {
	if dir == "" {
		return nil, SecretsIndex{}, errors.New("backup: no configuration directory is known")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, SecretsIndex{}, fmt.Errorf("backup: reading %s: %w", dir, err)
	}

	wanted := map[string]bool{}
	for _, name := range SecretsAlso {
		wanted[name] = true
	}

	var index SecretsIndex
	var files []SecretFile
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		match, err := filepath.Match(SecretsPattern, name)
		if err != nil {
			return nil, SecretsIndex{}, fmt.Errorf("backup: %q: %w", SecretsPattern, err)
		}
		if !match && !wanted[name] {
			continue
		}

		// Lstat, not Stat, and regular files only.
		//
		// A symlink in here would be read *through*, and what it
		// pointed at would land inside a file the customer is allowed
		// to ask for. /etc/shadow, another customer's configuration, a
		// private key. The directory is root-owned and nothing should
		// be able to plant one; "should" is why this is two lines
		// rather than a comment saying it cannot happen.
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			index.Skipped = append(index.Skipped, SecretsSkip{Name: name, Reason: err.Error()})
			continue
		}
		if !info.Mode().IsRegular() {
			index.Skipped = append(index.Skipped, SecretsSkip{
				Name:   name,
				Reason: "not a regular file (" + info.Mode().Type().String() + ")",
			})
			continue
		}
		if info.Size() > MaxSecretFileBytes {
			return nil, SecretsIndex{}, fmt.Errorf("backup: %s is %d bytes and the limit "+
				"for one configuration file is %d. Nothing was written; a secrets backup is "+
				"the configuration, and something that large in there is not configuration",
				name, info.Size(), MaxSecretFileBytes)
		}

		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			// Recorded, not fatal. A file this account cannot read is
			// the common case on a machine where the configuration is
			// split across two groups, and a backup of the other five
			// files is worth far more than no backup at all.
			index.Skipped = append(index.Skipped, SecretsSkip{Name: name, Reason: err.Error()})
			continue
		}
		total += int64(len(body))
		if total > MaxSecretsBytes {
			return nil, SecretsIndex{}, fmt.Errorf("backup: the files in %s come to more "+
				"than %d bytes. Nothing was written", dir, MaxSecretsBytes)
		}

		sum := sha256.Sum256(body)
		files = append(files, SecretFile{Name: name, Mode: info.Mode().Perm(), Bytes: body})
		index.Files = append(index.Files, SecretsEntry{
			Name:   name,
			Bytes:  int64(len(body)),
			Mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}

	if len(files) == 0 {
		return nil, SecretsIndex{}, fmt.Errorf("backup: nothing in %s could be read. A "+
			"secrets backup with no configuration in it would be a file that reports "+
			"success and restores nothing", dir)
	}
	// Sorted so the same directory always produces the same file.
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	sort.Slice(index.Files, func(i, j int) bool { return index.Files[i].Name < index.Files[j].Name })
	sort.Slice(index.Skipped, func(i, j int) bool { return index.Skipped[i].Name < index.Skipped[j].Name })
	return files, index, nil
}

// PeekSecrets reads the plaintext half of a secrets file.
//
// No password, and no decryption. It answers "what is this file and
// which recipient opens it", which is what somebody holding an
// unlabelled backup needs before they can do anything else.
func PeekSecrets(r io.Reader) (SecretsManifest, error) {
	m, _, err := readSecrets(r)
	return m, err
}

// OpenSecrets decrypts a secrets file with the developer password.
func OpenSecrets(r io.Reader, password string) (Secrets, error) {
	manifest, box, err := readSecrets(r)
	if err != nil {
		return Secrets{}, err
	}
	recipient, err := devseal.ParseRecipient(manifest.Recipient)
	if err != nil {
		return Secrets{}, fmt.Errorf("backup: the recipient in this file: %w", err)
	}
	identity, err := devseal.Reopen(password, recipient)
	if err != nil {
		return Secrets{}, err
	}
	inner, err := identity.Open(secretsHeader(manifest), manifest.Ephemeral, box)
	if err != nil {
		return Secrets{}, fmt.Errorf("backup: opening the sealed configuration: %w", err)
	}

	out := Secrets{Manifest: manifest}
	tr := tar.NewReader(bytes.NewReader(inner))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Secrets{}, fmt.Errorf("backup: reading the sealed archive: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(tr, MaxSecretFileBytes+1))
		if err != nil {
			return Secrets{}, fmt.Errorf("backup: reading %s: %w", hdr.Name, err)
		}
		switch {
		case hdr.Name == SecretsIndexName:
			if err := json.Unmarshal(body, &out.Index); err != nil {
				return Secrets{}, fmt.Errorf("backup: reading the listing: %w", err)
			}
		case strings.HasPrefix(hdr.Name, SecretsFileDir):
			out.Files = append(out.Files, SecretFile{
				Name:  strings.TrimPrefix(hdr.Name, SecretsFileDir),
				Mode:  fs.FileMode(hdr.Mode).Perm(),
				Bytes: body,
			})
		}
	}
	return out, nil
}

// readSecrets pulls the two outer members apart.
func readSecrets(r io.Reader) (SecretsManifest, string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return SecretsManifest{}, "", fmt.Errorf("backup: this file is not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	var manifest SecretsManifest
	var seenManifest bool
	var box string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SecretsManifest{}, "", fmt.Errorf("backup: reading the archive: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(tr, MaxSecretsBytes*2))
		if err != nil {
			return SecretsManifest{}, "", fmt.Errorf("backup: reading %s: %w", hdr.Name, err)
		}
		switch hdr.Name {
		case SecretsManifestName:
			if err := json.Unmarshal(body, &manifest); err != nil {
				return SecretsManifest{}, "", fmt.Errorf("backup: reading the manifest: %w", err)
			}
			seenManifest = true
		case SecretsPayloadName:
			box = string(body)
		}
	}
	if !seenManifest {
		return SecretsManifest{}, "", fmt.Errorf("backup: this file has no %s; it is not a "+
			"secrets backup", SecretsManifestName)
	}
	if box == "" {
		return SecretsManifest{}, "", fmt.Errorf("backup: this file has no %s", SecretsPayloadName)
	}
	return manifest, box, nil
}

// MeasureSecrets is the disk check before a secrets backup.
//
// The same shape as Measure and for the same reason, even though the
// numbers are three orders of magnitude smaller: the refusal has to
// come from one place, and a file this package writes without asking
// whether it fits is a file that can fill the disk the collector is
// running on.
//
// The estimate is the plaintext size rather than a fraction of it. This
// payload is encrypted, so it does not compress, and base64 makes it a
// third larger again - a guess in the other direction from the data
// backup's, arrived at the same way: err towards refusing.
func MeasureSecrets(dir string, files []SecretFile) (Estimate, error) {
	var total int64
	for _, f := range files {
		total += int64(len(f.Bytes))
	}
	space, err := diskspace.Read(parentOf(dir))
	if err != nil {
		return Estimate{}, fmt.Errorf("backup: reading free space for %s: %w", dir, err)
	}
	margin := int64(FreeMargin)
	if space.TotalBytes > 0 && space.TotalBytes/10 < margin {
		margin = space.TotalBytes / 10
	}
	return Estimate{
		TableBytes: total,
		FileBytes:  total * 2,
		AvailBytes: space.AvailBytes,
		Margin:     margin,
	}, nil
}

// writeSecretEntry is writeEntry with the file's own mode kept.
//
// The mode travels because a restore has to put it back. Writing
// upgrader.toml back at 0644 because the archive did not remember would
// hand the schema_admin credential to every account on the machine, at
// the end of the one procedure nobody double-checks.
func writeSecretEntry(tw *tar.Writer, name string, mode fs.FileMode, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     int64(mode.Perm()),
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("backup: writing %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("backup: writing %s: %w", name, err)
	}
	return nil
}
