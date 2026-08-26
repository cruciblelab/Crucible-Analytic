package ja4

import (
	"path/filepath"
	"testing"
)

// Fuzzing the two functions in this repository that read bytes chosen by
// a stranger.
//
// # Why these two, and why it matters more here than in most projects
//
// The collector is a TCP/TLS proxy sitting in front of the customer's
// website. Everything a visitor sends arrives at these functions before
// anything has been validated, authenticated or even decrypted -
// ParseClientHelloFromRecords is handed the first bytes off the socket.
//
// And Go kills the whole process on an unrecovered panic, in any
// goroutine. internal/proxy's accept loop spawns a bare goroutine per
// connection with no recover() anywhere on the path, so a malformed
// ClientHello that panicked in ParseClientHello would not merely lose one
// connection: it would stop the collector, and the collector stopping
// takes the customer's website offline. That is a denial of service
// available to anybody who can open a TCP connection, which is everybody.
//
// The parser is written defensively - every read goes through cursor.go
// and returns an ok - but "written defensively" is a description of
// intent. This is the measurement.
//
// The seed corpus is the five real FoxIO reference captures (see
// testdata/README.md) plus the shapes that break a parser written without
// bounds checks. Starting from real handshakes matters: the mutator
// reaches deep into extension parsing far sooner from a valid hello than
// it ever would from random bytes.
//
// Run longer than the seed corpus:
//
//	go test -run XXX -fuzz FuzzParseClientHelloFromRecords ./internal/ja4/

// fixtureHellos returns the raw record-framed bytes of every checked-in
// FoxIO capture. Loaded by glob rather than a hardcoded list so a fixture
// added later is fuzzed without anyone remembering to come back here.
func fixtureHellos(tb testing.TB) [][]byte {
	tb.Helper()
	paths, err := filepath.Glob("testdata/*.hex")
	if err != nil {
		tb.Fatalf("globbing fixtures: %v", err)
	}
	if len(paths) == 0 {
		tb.Fatal("no testdata/*.hex fixtures found; the fuzz seed corpus would be nothing but hand-written scraps")
	}
	out := make([][]byte, 0, len(paths))
	for _, p := range paths {
		out = append(out, readHexFixture(tb, p))
	}
	return out
}

// FuzzParseClientHelloFromRecords takes raw bytes exactly as they arrive
// off the wire: TLS record framing included, nothing stripped. This is
// internal/fullproxy's path, where the bytes come from snoopConn.
func FuzzParseClientHelloFromRecords(f *testing.F) {
	// The shapes that break a parser written without bounds checks:
	// empty, truncated at every layer, and lengths that claim more than
	// the buffer holds.
	f.Add([]byte{})
	f.Add([]byte{0x16})
	f.Add([]byte{0x16, 0x03, 0x01})
	f.Add([]byte{0x16, 0x03, 0x01, 0xff, 0xff})                         // record claims more than it carries
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x04, 0x01, 0xff, 0xff, 0xff}) // handshake ditto

	for _, raw := range fixtureHellos(f) {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		// The contract is only that it returns. A parse error is a
		// perfectly good answer to nonsense; a panic is not.
		ch, err := ParseClientHelloFromRecords(raw)
		if err != nil {
			return
		}
		if ch == nil {
			t.Fatal("no error and no ClientHello; a caller would dereference nil")
		}
		// Fingerprinting is the next thing the proxy does with it, on the
		// same goroutine, so a panic there reaches the same process. Its
		// return value is not asserted on: Fingerprint builds fixed-width
		// tokens with fmt.Sprintf and cannot return "", so a check for
		// that would be a check that can never fire. Running it is the
		// point.
		Fingerprint(ch)
	})
}

// FuzzParseClientHello skips the record layer and hands the handshake
// message straight in - internal/proxy's path, and the one with no
// recover() between it and the process exiting.
func FuzzParseClientHello(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte{0x01, 0x00, 0x00, 0x00})
	f.Add([]byte{0x01, 0xff, 0xff, 0xff}) // claims a length nothing backs

	// The fixtures are record-framed, so strip the 5-byte record header to
	// get the handshake message this function actually expects - the same
	// [5:] the FoxIO reference tests use.
	for _, raw := range fixtureHellos(f) {
		if len(raw) > recordHeaderLen {
			f.Add(raw[recordHeaderLen:])
		}
	}

	f.Fuzz(func(t *testing.T, msg []byte) {
		ch, err := ParseClientHello(msg)
		if err != nil {
			return
		}
		if ch == nil {
			t.Fatal("no error and no ClientHello; a caller would dereference nil")
		}
		Fingerprint(ch)
	})
}
