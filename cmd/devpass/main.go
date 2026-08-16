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
// Usage:
//
//	go run ./cmd/devpass                 # prompts, no echo
//	echo -n "password" | go run ./cmd/devpass -stdin
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

func main() {
	fromStdin := flag.Bool("stdin", false, "read the password from standard input instead of prompting")
	flag.Parse()

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
