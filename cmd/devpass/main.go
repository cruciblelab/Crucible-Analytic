// Command devpass turns a developer password into the hash to paste
// into a config file.
//
// The settings that carry legal weight - whether IP addresses are stored
// whole, how long visit records are kept, which campaign parameters
// reach the disk - require a password on every change. That password
// lives in the config file, and only ever as an argon2id hash:
//
//	[developer]
//	password_hash = "$argon2id$v=19$m=19456,t=2,p=1$..."
//
// This exists because there has to be a way to produce that line, and
// the obvious alternatives are worse. A plaintext field the server
// hashes on startup would mean the password sat readable on disk, in
// backups, and in whatever the operator pasted it from. A panel form
// would mean the panel could set the password that limits the panel.
//
// # And the other two things the same password does
//
// The secrets backup is sealed to this password, and this command is
// both ends of that: `-recipient` produces the public half for
// upgrader.toml, and `-open` decrypts a backup with the password
// itself.
//
// Here rather than in the upgrader because of when `-open` is used. A
// secrets backup is opened on the day the machine is gone: there is no
// installation to run a subcommand of, no config file to read, and no
// database to connect to. What there is, is this one static binary and
// something in somebody's head.
//
// Usage:
//
//	go run ./cmd/devpass                 # prompts, no echo
//	echo -n "password" | go run ./cmd/devpass -stdin
//	go run ./cmd/devpass -recipient      # the [backup] recipient line
//	go run ./cmd/devpass -open sirlar-....tar.gz -into ./kurtarma
package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/devseal"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/devpass
//
// Left empty when it was not: internal/buildinfo falls back to the commit
// Go embeds into every build made from a working tree, so an unstamped
// binary still answers "which build is this" with something true.
var version string

func main() {
	fromStdin := flag.Bool("stdin", false, "read the password from standard input instead of prompting")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	ipKey := flag.Bool("ipkey", false, "generate a random privacy.ip_hash_key instead of hashing a password")
	recipient := flag.Bool("recipient", false,
		"generate the [backup] recipient line that secrets backups are sealed to")
	open := flag.String("open", "", "open a secrets backup file with the developer password")
	into := flag.String("into", "", "the directory `-open` writes the recovered files into")
	flag.Parse()

	// Before the config is read, and before anything can fail: this is the
	// question asked when a process will not start, so it must not need a
	// working installation to answer.
	if *showVersion {
		buildinfo.Print(os.Stdout, "devpass", version)
		return
	}

	if *ipKey {
		if err := printIPKey(); err != nil {
			fmt.Fprintf(os.Stderr, "devpass: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Opening a backup asks for the password once, not twice: there is
	// nothing to confirm, because the file already knows what the right
	// answer is and says so immediately.
	if *open != "" {
		if err := openBackup(*open, *into, *fromStdin); err != nil {
			fmt.Fprintf(os.Stderr, "devpass: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *into != "" {
		fmt.Fprintln(os.Stderr, "devpass: -into means nothing without -open")
		os.Exit(1)
	}

	password, err := readPassword(*fromStdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "devpass: %v\n", err)
		os.Exit(1)
	}

	if n := utf8.RuneCountInString(password); n < devgate.MinPasswordLen {
		// Longer than an account password's minimum, on purpose: this is
		// one shared secret, typed rarely, protecting the settings with
		// legal consequences. There is no usability argument for keeping
		// it short and no lockout risk in making it long.
		fmt.Fprintf(os.Stderr,
			"devpass: password is %d characters; at least %d are required\n", n, devgate.MinPasswordLen)
		os.Exit(1)
	}

	if *recipient {
		if err := printRecipient(password); err != nil {
			fmt.Fprintf(os.Stderr, "devpass: %v\n", err)
			os.Exit(1)
		}
		return
	}

	hash, err := argon2id.Hash(password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "devpass: %v\n", err)
		os.Exit(1)
	}

	// The hash alone on stdout, so it can be piped. Everything else goes
	// to stderr - a command whose output cannot be redirected cleanly
	// gets copied by hand, and copying by hand is how a character goes
	// missing.
	fmt.Println(hash)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Yapılandırma dosyasına ekleyin:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  [developer]")
	fmt.Fprintf(os.Stderr, "  password_hash = %q\n", hash)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Şifrenin kendisini hiçbir yere yazmayın: yalnız yukarıdaki hash saklanır.")
	fmt.Fprintln(os.Stderr, "Şifreyi kaybederseniz yenisini üretip bu satırı değiştirin - hash'ten geri")
	fmt.Fprintln(os.Stderr, "döndürülemez, ki zaten amacı budur.")
}

// readPassword takes the password without echoing it where it can, and
// says so where it cannot.
func readPassword(fromStdin bool) (string, error) {
	if fromStdin {
		// Not trimmed beyond the line ending: a leading or trailing
		// space inside a password is legitimate, and silently removing
		// it would produce a hash that never matches what the person
		// thinks they set.
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("standard input is not a terminal; use -stdin to pipe the password in")
	}

	fmt.Fprint(os.Stderr, "Geliştirici şifresi: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	fmt.Fprint(os.Stderr, "Tekrar: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	// Confirmed, because a mistyped password here is not discovered at
	// the next login - it is discovered months later by somebody who
	// needs to change a setting and cannot.
	if string(first) != string(second) {
		return "", fmt.Errorf("the two entries did not match")
	}
	return string(first), nil
}

// printRecipient emits the [backup] recipient line.
//
// # Why the same password, and not a second one
//
// Because the alternative is a second secret the developer has to keep
// somewhere, and a secret kept somewhere is a secret that gets lost.
// The developer password already exists, is already required for every
// change with legal weight, and is already the thing this product means
// by "the developer is here". Making the backup answer to it means
// there is one thing to remember and nothing new to store.
//
// The password itself never leaves this process. What is printed is a
// public key: it goes in a config file, it travels inside every secrets
// backup, and it opens nothing.
//
// # What it costs to lose the password
//
// Every secrets backup already taken. That is the honest trade and it
// is printed below rather than left to be discovered - the whole design
// is that nothing on the machine can open those files, and "nothing"
// includes the operator standing at the machine.
func printRecipient(password string) error {
	identity, err := devseal.Generate(password)
	if err != nil {
		return err
	}
	line := identity.Recipient().String()

	// The line alone on stdout, so it can be piped; everything else to
	// stderr. Same reason as the hash above.
	fmt.Println(line)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "upgrader.toml dosyasına ekleyin:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  [backup]")
	fmt.Fprintf(os.Stderr, "  recipient = %q\n", line)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Bu satır açık anahtardır: sırlar yedeğini yalnız kapatmaya yarar,")
	fmt.Fprintln(os.Stderr, "açmaya yaramaz. Açan tek şey bu parolanın kendisidir ve o parola")
	fmt.Fprintln(os.Stderr, "sunucuda hiçbir yerde yazılı değildir.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Parolayı kaybederseniz alınmış bütün sırlar yedekleri açılamaz hâle")
	fmt.Fprintln(os.Stderr, "gelir — root bile açamaz, tasarım budur. Yeni bir parola üretip bu")
	fmt.Fprintln(os.Stderr, "satırı değiştirmek yalnız bundan sonrakileri kurtarır.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Geliştirici parolasıyla aynı parolayı kullanın: password_hash ile bu")
	fmt.Fprintln(os.Stderr, "satır aynı parolanın iki ayrı türevidir, birbirlerinden üretilemezler.")
	return nil
}

// openBackup decrypts a secrets backup and writes what was in it.
//
// # Why this is in devpass and not in the upgrader
//
// Because of when it is used. A secrets backup is opened on the day the
// machine is gone: there is no installation to run a subcommand of, no
// config file to read, and no database to connect to. What there is, is
// this one static binary and the password.
//
// So it takes a path and a directory and needs nothing else - not even
// the recipient, which travels inside the file for exactly this reason.
func openBackup(path, into string, fromStdin bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read before asking for anything. The file names its own recipient
	// and its own date, and somebody holding an unlabelled backup needs
	// to see that before they start typing a password at it.
	head, err := backup.PeekSecrets(f)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Dosya   : %s\n", path)
	fmt.Fprintf(os.Stderr, "Alındı  : %s\n", head.TakenAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "Sürüm   : %s (şema %d)\n", head.BinaryVersion, head.SchemaVersion)
	fmt.Fprintf(os.Stderr, "Alıcı   : %s\n\n", head.Recipient)

	if into == "" {
		return errors.New("say where the files should go with -into <dizin>")
	}
	// Refused before the password is typed, not after. Deriving the key
	// costs a real fraction of a second and 128 MiB, and being told
	// afterwards that the destination is unusable is being told at the
	// wrong end of the operation.
	if err := prepareInto(into); err != nil {
		return err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind %s: %w", path, err)
	}
	password, err := readOnePassword(fromStdin)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Anahtar türetiliyor…")
	secrets, err := backup.OpenSecrets(f, password)
	if err != nil {
		return err
	}

	for _, file := range secrets.Files {
		// filepath.Base, because a name is a name. The archive was
		// written by this project and its entries are bare filenames,
		// but an archive is a file somebody can hand you, and "../.."
		// in an entry name is the oldest way to write outside the
		// directory you were pointed at.
		dst := filepath.Join(into, filepath.Base(file.Name))
		mode := file.Mode.Perm()
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(dst, file.Bytes, mode); err != nil {
			return err
		}
		// WriteFile applies the mode only when it creates the file, and
		// prepareInto has already refused a directory with anything in
		// it - so this is belt and braces on the one thing that must
		// not come back wrong. upgrader.toml written at 0644 would hand
		// the schema_admin credential to every account on the machine.
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  %-24s %6d bayt  %04o\n", file.Name, len(file.Bytes), mode)
	}
	for _, skip := range secrets.Index.Skipped {
		fmt.Fprintf(os.Stderr, "  %-24s ALINAMAMIŞ: %s\n", skip.Name, skip.Reason)
	}

	fmt.Fprintf(os.Stderr, "\n%d dosya %s dizinine yazıldı.\n", len(secrets.Files), into)
	fmt.Fprintln(os.Stderr, "Sahipliği ve grubu kurulumun beklediği hâle getirmeyi unutmayın;")
	fmt.Fprintln(os.Stderr, "arşiv kipleri taşır, sahipleri taşımaz.")
	return nil
}

// prepareInto creates the destination and refuses to write into one
// that already has something in it.
//
// Not a convenience. This writes configuration files with real
// credentials in them; landing them on top of a live /etc directory
// because somebody typed the wrong path is the kind of mistake that
// happens on the day everything else has already gone wrong.
func prepareInto(into string) error {
	if err := os.MkdirAll(into, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(into, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(into)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty. Point -into at a new directory: these files "+
			"carry live credentials and writing them over something else is not "+
			"recoverable", into)
	}
	return nil
}

// readOnePassword asks once, with no confirmation.
func readOnePassword(fromStdin bool) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("standard input is not a terminal; use -stdin to pipe the password in")
	}
	fmt.Fprint(os.Stderr, "Geliştirici şifresi: ")
	typed, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(typed), nil
}

// printIPKey emits a random key for privacy.ip_hash_key.
//
// Generated rather than typed, and generated here rather than by each
// service at startup, because both writers must carry the *same* key:
// they write the two halves of the crossover join, and different keys
// make that join find nothing with no error to say why. One value, one
// place it came from, copied into both files.
//
// It is not a password and is never typed by a person, so it is drawn
// from the system's randomness at full length rather than being
// something memorable.
func printIPKey() error {
	key := make([]byte, privacy.MinHashKeyLen)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("draw key: %w", err)
	}
	encoded := base64.RawStdEncoding.EncodeToString(key)

	fmt.Println(encoded)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Bu anahtarı HEM collector HEM beacon yapılandırmasına, aynen ekleyin:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  [privacy]")
	fmt.Fprintln(os.Stderr, `  ip_storage   = "full"`)
	fmt.Fprintf(os.Stderr, "  ip_hash_key  = %q\n", encoded)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "İki dosyada farklı olursa kesişim birleşimi sessizce boş döner — hata")
	fmt.Fprintln(os.Stderr, "vermez, yalnızca sayılar sıfırlanır. Aynı olduğundan emin olun.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Anahtar yalnızca full modda kullanılır. Maskeli modda hiçbir işlevi")
	fmt.Fprintln(os.Stderr, "yoktur ve gerekmez.")
	return nil
}
