// The encryption verdict, all three answers, on any machine.
//
// # Why this is a unit test and not only an integration one
//
// One of the three branches could not be reached on a developer's
// machine: it needs a database on another host, and a laptop running a
// local PostgreSQL does not have one. So it was exercised only on CI -
// where the check behaved correctly and the *test* did not, because the
// test asserted the local answer on the strength of a comment reading
// "the connection this suite uses is to localhost".
//
// That comment was an assumption, written down and never checked, and it
// was false in the one environment that mattered. The merge gate had
// been red on every push for two independent reasons and this was the
// second of them.
//
// A pure function needs neither a remote database nor a laptop's luck.
//
// No build tag: no database, no network.
package preflight

import (
	"strings"
	"testing"
)

func addr(s string) *string { return &s }

func TestTheEncryptionVerdictCoversAllThreeAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		encrypted  bool
		serverAddr *string
		wantStatus CheckStatus
		wantSays   string
		wantNot    string
		why        string
	}{
		{
			name:      "TLS ile şifreli, uzak sunucu",
			encrypted: true, serverAddr: addr("10.0.0.5"),
			wantStatus: CheckPass, wantSays: "TLS",
			why: "encryption is the reason, and it is the only one that actually is",
		},
		{
			name:      "TLS ile şifreli, yerel sunucu",
			encrypted: true, serverAddr: addr("127.0.0.1"),
			wantStatus: CheckPass, wantSays: "TLS",
			why: "encrypted wins over local: it is the stronger fact",
		},
		{
			// A unix socket. inet_server_addr() is NULL, which is not
			// the same as an address that failed to parse and must not
			// be treated as remote.
			name:      "unix soketi, adres yok",
			encrypted: false, serverAddr: nil,
			wantStatus: CheckPass, wantSays: "bu makinede", wantNot: "TLS ile şifreli",
			why: "a socket never reaches a network interface",
		},
		{
			name:      "loopback, şifresiz",
			encrypted: false, serverAddr: addr("127.0.0.1"),
			wantStatus: CheckPass, wantSays: "bu makinede", wantNot: "TLS ile şifreli",
			why: "the most common deployment there is; a warning here teaches people to ignore warnings",
		},
		{
			name:      "IPv6 loopback, şifresiz",
			encrypted: false, serverAddr: addr("::1"),
			wantStatus: CheckPass, wantSays: "bu makinede",
			why: "loopback is loopback in either family",
		},
		{
			// The branch no laptop can reach, and the one CI is.
			name:      "uzak sunucu, şifresiz — CI'ın durumu",
			encrypted: false, serverAddr: addr("172.18.0.2"),
			wantStatus: CheckWarn, wantSays: "172.18.0.2",
			why: "the password and every analytics row cross a network in the clear",
		},
		{
			// host() should never hand us this, but a value that does
			// not parse must not be quietly called local - that is the
			// direction that fails open.
			name:      "ayrıştırılamayan adres",
			encrypted: false, serverAddr: addr("bu-bir-adres-degil"),
			wantStatus: CheckWarn, wantSays: "bu-bir-adres-degil",
			why: "an address nobody can read is not a reason to assume safety",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, detail, fix := encryptionVerdict(tc.encrypted, tc.serverAddr)

			if status != tc.wantStatus {
				t.Errorf("status = %s, want %s — %s", status, tc.wantStatus, tc.why)
			}
			if !strings.Contains(detail, tc.wantSays) {
				t.Errorf("the detail does not mention %q: %s", tc.wantSays, detail)
			}
			if tc.wantNot != "" && strings.Contains(detail, tc.wantNot) {
				t.Errorf("the detail claims %q, which is not why it passed: %s", tc.wantNot, detail)
			}

			// A warning without a fix is a sentence that ends by telling
			// somebody they have a problem.
			if status == CheckWarn && strings.TrimSpace(fix) == "" {
				t.Error("a warning with no fix")
			}
			if status == CheckPass && strings.TrimSpace(fix) != "" {
				t.Errorf("a passing check offered a fix: %s", fix)
			}
		})
	}
}

// TestALocalPassNeverClaimsEncryption.
//
// The two passes exist for different reasons and only one of them is
// about encryption. A check that said "encrypted" for a loopback
// connection would be telling every single-machine deployment something
// untrue - and it would be believed, because it is the reassuring
// answer.
func TestALocalPassNeverClaimsEncryption(t *testing.T) {
	for _, a := range []*string{nil, addr("127.0.0.1"), addr("::1"), addr("0.0.0.0")} {
		_, detail, _ := encryptionVerdict(false, a)
		if strings.Contains(detail, "şifreli") {
			where := "unix soketi"
			if a != nil {
				where = *a
			}
			t.Errorf("%s: an unencrypted connection was described as encrypted: %s", where, detail)
		}
	}
}
