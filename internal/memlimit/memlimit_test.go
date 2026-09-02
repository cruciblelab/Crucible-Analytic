package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeRoot builds a filesystem for detect to read.
func fakeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root + "/"
}

const meminfo16G = `MemTotal:       16461068 kB
MemFree:         1234567 kB
MemAvailable:   15893140 kB
`

// TestDetect.
//
// # The values here were measured, not invented
//
// 268435456 is what `docker run --memory=256m` writes, read out of the
// container on a cgroup v1 host. 9223372036854771712 is what the same
// file says for a process with no limit at all, read on the same host.
// The meminfo block is this machine's, trimmed.
//
// That matters for the two rows that look like edge cases and are not:
// "unlimited v1" is the ordinary state of every process that is not in a
// constrained container, and getting it wrong means telling a 16 GB
// machine it has 8 exabytes - which passes every profile check and
// defeats the point.
func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  Limit
	}{
		{
			name: "a container limit under cgroup v2",
			files: map[string]string{
				"sys/fs/cgroup/memory.max": "268435456\n",
				"proc/meminfo":             meminfo16G,
			},
			want: Limit{Bytes: 268435456, From: SourceCgroupV2},
		},
		{
			name: "a container limit under cgroup v1",
			files: map[string]string{
				"sys/fs/cgroup/memory/memory.limit_in_bytes": "268435456\n",
				"proc/meminfo": meminfo16G,
			},
			want: Limit{Bytes: 268435456, From: SourceCgroupV1},
		},
		{
			name: "v2 says max, meaning no limit",
			files: map[string]string{
				"sys/fs/cgroup/memory.max": "max\n",
				"proc/meminfo":             meminfo16G,
			},
			want: Limit{Bytes: 15893140 * 1024, From: SourceAvailable},
		},
		{
			name: "v1 says the largest number that fits, meaning no limit",
			files: map[string]string{
				"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
				"proc/meminfo": meminfo16G,
			},
			want: Limit{Bytes: 15893140 * 1024, From: SourceAvailable},
		},
		{
			name: "v2 is present and unlimited while v1 carries a real limit",
			files: map[string]string{
				"sys/fs/cgroup/memory.max":                   "max\n",
				"sys/fs/cgroup/memory/memory.limit_in_bytes": "268435456\n",
				"proc/meminfo":                               meminfo16G,
			},
			want: Limit{Bytes: 268435456, From: SourceCgroupV1},
		},
		{
			name:  "nothing readable at all",
			files: map[string]string{},
			want:  Limit{From: SourceUnknown},
		},
		{
			name: "a cgroup file with no meminfo to sanity-check it against",
			files: map[string]string{
				"sys/fs/cgroup/memory.max": "268435456\n",
			},
			want: Limit{Bytes: 268435456, From: SourceCgroupV2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detect(fakeRoot(t, tc.files))
			if got != tc.want {
				t.Errorf("detect() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestMemAvailableIsPreferredToMemTotal.
//
// Its own test because it is the decision most likely to be "simplified"
// by somebody who reads MemTotal as the obvious field.
//
// This product ships onto one VDS running the collector, the beacon, the
// API, the panel and TimescaleDB together, and timescaledb-tune sizes
// shared_buffers at roughly a quarter of RAM before anything else asks
// for any. MemTotal would report memory that is already spoken for, and
// the profile check exists precisely to stop somebody choosing a size
// the machine cannot give them.
func TestMemAvailableIsPreferredToMemTotal(t *testing.T) {
	root := fakeRoot(t, map[string]string{
		"proc/meminfo": `MemTotal:       16461068 kB
MemAvailable:    2000000 kB
`,
	})
	got := detect(root)
	if got.From != SourceAvailable {
		t.Fatalf("From = %q, want %q", got.From, SourceAvailable)
	}
	if want := uint64(2000000) * 1024; got.Bytes != want {
		t.Errorf("Bytes = %d, want %d (MemAvailable, not MemTotal - a machine whose "+
			"database already holds most of its RAM has nothing like MemTotal to give)",
			got.Bytes, want)
	}
}

// TestAUnitOtherThanKilobytesIsRefused.
//
// /proc/meminfo has spelled every size in kB for the whole life of the
// file, so this is defensive. It is here because of the direction the
// mistake runs: reading a kB figure as bytes makes the machine look a
// thousand times smaller, and a ceiling a thousand times too small
// refuses every profile - which arrives as "the panel will not let me
// change anything" and has nothing to do with memory.
func TestAUnitOtherThanKilobytesIsRefused(t *testing.T) {
	root := fakeRoot(t, map[string]string{
		"proc/meminfo": "MemAvailable:    2000000 MB\n",
	})
	if got := detect(root); got.From != SourceUnknown {
		t.Errorf("detect() = %+v, want unknown: the unit was not kB and the number "+
			"would have been wrong by a factor of a thousand", got)
	}
}

// TestDetectOnThisMachine is a smoke test against the real filesystem.
//
// It asserts almost nothing on purpose. What it is for is the case the
// table above cannot reach: a fixture proves the parser, and only the
// real /sys and /proc prove that the paths are the ones this kernel
// actually uses. If a future cgroup layout moves them, every test above
// still passes and this one starts saying "unknown".
func TestDetectOnThisMachine(t *testing.T) {
	got := Detect()
	t.Logf("this machine reports %d bytes (%.1f MB) from %s",
		got.Bytes, float64(got.Bytes)/(1<<20), got.From)
	if !got.Known() {
		t.Skip("no ceiling readable here, which is a fact about this machine " +
			"rather than a failure - but if this skips in CI on Linux, the paths " +
			"above have moved")
	}
	if got.Bytes < 16<<20 {
		t.Errorf("this process is limited to %d bytes, which is less than the Go "+
			"runtime needs to have got this far; the number is being read wrong",
			got.Bytes)
	}
}
