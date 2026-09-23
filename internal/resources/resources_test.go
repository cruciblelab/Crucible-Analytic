package resources

import (
	"context"
	"io/fs"
	"runtime"
	"runtime/debug"
	"testing"
)

// fakeFS is a hierarchy this machine does not have, handed to the
// detector as its only view of /proc and /sys.
type fakeFS map[string]string

func (f fakeFS) read(path string) ([]byte, error) {
	s, ok := f[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(s), nil
}

// v1Mounts is this machine's own mountinfo shape, trimmed to the lines
// that matter: every v1 controller is its own mount, and there is no
// cgroup2 line at all.
const v1Mounts = `38 37 0:30 / /sys/fs/cgroup/cpu rw,relatime - cgroup cgroup rw,cpu
41 37 0:33 / /sys/fs/cgroup/memory rw,relatime - cgroup cgroup rw,memory
`

// v1Sentinel is what cgroup v1 writes for "no limit", copied from a real
// hierarchy rather than computed - a test that derives it from the
// constant it is checking would agree with a wrong constant.
const v1Sentinel = "9223372036854771712\n"

func TestTheRealHierarchyThisWasWrittenAgainst(t *testing.T) {
	// Read off this container on 2026-09-23: the leaf carries the limit,
	// every ancestor carries the sentinel, and /proc/self/cgroup has a
	// "0::/" line for a cgroup2 filesystem that is not mounted.
	leaf := "/process_api/01a0cd47/claude-code-bash"
	f := fakeFS{
		"/proc/self/cgroup":    "9:name=systemd:/\n4:memory:" + leaf + "\n1:cpu:/\n0::/\n",
		"/proc/self/mountinfo": v1Mounts,
		"/sys/fs/cgroup/memory" + leaf + "/memory.limit_in_bytes":          "14345912320\n",
		"/sys/fs/cgroup/memory/process_api/01a0cd47/memory.limit_in_bytes": v1Sentinel,
		"/sys/fs/cgroup/memory/process_api/memory.limit_in_bytes":          v1Sentinel,
		"/sys/fs/cgroup/memory/memory.limit_in_bytes":                      v1Sentinel,
	}
	got := detectMemory(f.read)
	if got.Bytes != 14345912320 || got.Source != "cgroup v1" {
		t.Errorf("got %+v, want 14345912320 from cgroup v1", got)
	}
}

// The smallest limit on the path wins, wherever on the path it sits. A
// leaf that says "no limit" can still be killed by its parent's.
func TestAnAncestorsSmallerLimitWins(t *testing.T) {
	f := fakeFS{
		"/proc/self/cgroup":    "4:memory:/svc/beacon\n",
		"/proc/self/mountinfo": v1Mounts,
		"/sys/fs/cgroup/memory/svc/beacon/memory.limit_in_bytes": v1Sentinel,
		"/sys/fs/cgroup/memory/svc/memory.limit_in_bytes":        "536870912\n",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes":            "1073741824\n",
	}
	if got := detectMemory(f.read); got.Bytes != 536870912 {
		t.Errorf("got %d, want the parent's 536870912 - the leaf is unlimited, the "+
			"parent is not, and the parent is what the kernel enforces", got.Bytes)
	}
}

func TestAHierarchyWithNoLimitAnywhereSaysNone(t *testing.T) {
	f := fakeFS{
		"/proc/self/cgroup":                             "4:memory:/a\n",
		"/proc/self/mountinfo":                          v1Mounts,
		"/sys/fs/cgroup/memory/a/memory.limit_in_bytes": v1Sentinel,
		"/sys/fs/cgroup/memory/memory.limit_in_bytes":   v1Sentinel,
	}
	if got := detectMemory(f.read); got.Bytes != 0 || got.Source != "" {
		t.Errorf("got %+v, want none", got)
	}
}

func TestCgroupV2(t *testing.T) {
	mounts := "30 25 0:26 / /sys/fs/cgroup rw,nosuid - cgroup2 cgroup2 rw\n"
	f := fakeFS{
		"/proc/self/cgroup":    "0::/system.slice/crucible-beacon.service\n",
		"/proc/self/mountinfo": mounts,
		"/sys/fs/cgroup/system.slice/crucible-beacon.service/memory.max": "268435456\n",
		"/sys/fs/cgroup/system.slice/memory.max":                         "max\n",
	}
	got := detectMemory(f.read)
	if got.Bytes != 268435456 || got.Source != "cgroup v2" {
		t.Errorf("got %+v, want 268435456 from cgroup v2 - this is the shape "+
			"systemd's MemoryMax= produces", got)
	}

	f["/sys/fs/cgroup/system.slice/crucible-beacon.service/memory.max"] = "max\n"
	if got := detectMemory(f.read); got.Bytes != 0 {
		t.Errorf(`"max" at every level read as %d, want none`, got.Bytes)
	}
}

// This machine's arrangement: a "0::/" line with no cgroup2 filesystem.
// Following the line into /sys/fs/cgroup would read a v1 directory as
// if it were the unified hierarchy.
func TestAV2LineWithNoV2MountIsNotFollowed(t *testing.T) {
	f := fakeFS{
		"/proc/self/cgroup":    "0::/\n",
		"/proc/self/mountinfo": v1Mounts,
		// A file a naive reader would find if it assumed v2 lives at
		// /sys/fs/cgroup whether or not anything is mounted there.
		"/sys/fs/cgroup/memory.max": "1048576\n",
	}
	if got := detectMemory(f.read); got.Bytes != 0 {
		t.Errorf("got %d from a cgroup2 filesystem that is not mounted", got.Bytes)
	}
}

// A container whose mount exposes a subtree: /proc/self/cgroup names the
// full path, mountinfo says the mount's root is that same path, and the
// limit lives at the mount point itself.
func TestTheMountRootIsTakenOffThePath(t *testing.T) {
	mounts := "41 37 0:33 /docker/abc /sys/fs/cgroup/memory ro - cgroup cgroup rw,memory\n"
	f := fakeFS{
		"/proc/self/cgroup":                           "4:memory:/docker/abc\n",
		"/proc/self/mountinfo":                        mounts,
		"/sys/fs/cgroup/memory/memory.limit_in_bytes": "134217728\n",
	}
	if got := detectMemory(f.read); got.Bytes != 134217728 {
		t.Errorf("got %d, want 134217728 - the path was not translated through "+
			"the mount root, so the limit at the mount point was never read", got.Bytes)
	}

	// The case above cannot tell a translated path from an untranslated
	// one, and a mutation that dropped the translation survived it: the
	// wrong path points into directories that do not exist, and the walk
	// up carries it back to the mount point, where the right file is.
	// Only a limit *below* the mount root separates them - the process
	// sits in a sub-group with its own tighter ceiling, which only the
	// translated path reaches.
	f["/proc/self/cgroup"] = "4:memory:/docker/abc/worker\n"
	f["/sys/fs/cgroup/memory/worker/memory.limit_in_bytes"] = "67108864\n"
	if got := detectMemory(f.read); got.Bytes != 67108864 {
		t.Errorf("got %d, want the sub-group's 67108864 - an untranslated path "+
			"never reaches it and falls back to the mount point's 134217728", got.Bytes)
	}
}

// A malformed file is no answer, not a zero-byte limit. Read as a limit,
// zero would have the runtime collect garbage without pause.
func TestAnUnreadableLimitIsNotALimit(t *testing.T) {
	for _, content := range []string{"", "0\n", "-1\n", "lots\n"} {
		f := fakeFS{
			"/proc/self/cgroup":                             "4:memory:/a\n",
			"/proc/self/mountinfo":                          v1Mounts,
			"/sys/fs/cgroup/memory/a/memory.limit_in_bytes": content,
		}
		if got := detectMemory(f.read); got.Bytes != 0 {
			t.Errorf("limit file %q read as %d, want no answer", content, got.Bytes)
		}
	}
	if got := detectMemory(fakeFS{}.read); got.Bytes != 0 {
		t.Errorf("no /proc at all read as %d", got.Bytes)
	}

	// And above a real limit. The loop above puts the bad value on the
	// leaf alone, where a zero is indistinguishable from "nothing read
	// yet" - and a mutation that accepted zero survived it. The order is
	// what matters: a real limit found first and a zero found on an
	// ancestor afterwards would win the comparison and erase the limit,
	// leaving a process that has one reporting that it has none.
	f := fakeFS{
		"/proc/self/cgroup":    "4:memory:/svc/beacon\n",
		"/proc/self/mountinfo": v1Mounts,
		"/sys/fs/cgroup/memory/svc/beacon/memory.limit_in_bytes": "268435456\n",
		"/sys/fs/cgroup/memory/svc/memory.limit_in_bytes":        "0\n",
	}
	if got := detectMemory(f.read); got.Bytes != 268435456 {
		t.Errorf("got %d, want the leaf's 268435456 - a zero on an ancestor "+
			"is no answer, and must not erase the limit found beneath it", got.Bytes)
	}
}

// keepMemoryLimit restores the runtime's limit after a test changes it.
func keepMemoryLimit(t *testing.T) {
	t.Helper()
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
}

func limitedTo(bytes string) fakeFS {
	return fakeFS{
		"/proc/self/cgroup":                             "4:memory:/a\n",
		"/proc/self/mountinfo":                          v1Mounts,
		"/sys/fs/cgroup/memory/a/memory.limit_in_bytes": bytes,
	}
}

func noEnv(string) string { return "" }

func TestACgroupLimitBecomesTheRuntimesSoftLimit(t *testing.T) {
	keepMemoryLimit(t)
	debug.SetMemoryLimit(1 << 62)

	b := apply(nil, limitedTo("1000000000\n").read, noEnv)

	// 80% of 1e9, spelled out.
	if got := debug.SetMemoryLimit(-1); got != 800000000 {
		t.Errorf("runtime limit is %d, want 800000000", got)
	}
	if b.GoMemLimit != 800000000 || b.GoMemLimitFrom != "cgroup" {
		t.Errorf("budget says %d from %q, want 800000000 from cgroup",
			b.GoMemLimit, b.GoMemLimitFrom)
	}
}

// The operator's GOMEMLIMIT wins, including "off". The runtime read it at
// start-up; overwriting it here would silently undo a decision somebody
// wrote into a unit file.
func TestTheOperatorsGOMEMLIMITIsNeverOverwritten(t *testing.T) {
	keepMemoryLimit(t)
	debug.SetMemoryLimit(123456789) // what the runtime made of the variable

	env := func(k string) string {
		if k == "GOMEMLIMIT" {
			return "123456789"
		}
		return ""
	}
	b := apply(nil, limitedTo("1000000000\n").read, env)

	if got := debug.SetMemoryLimit(-1); got != 123456789 {
		t.Errorf("runtime limit is %d, want the operator's 123456789 untouched", got)
	}
	if b.GoMemLimitFrom != "GOMEMLIMIT" {
		t.Errorf("budget says the limit came from %q", b.GoMemLimitFrom)
	}
}

func TestNoLimitMeansTheRuntimeIsLeftAlone(t *testing.T) {
	keepMemoryLimit(t)
	debug.SetMemoryLimit(1 << 62)

	b := apply(nil, fakeFS{}.read, noEnv)

	if got := debug.SetMemoryLimit(-1); got != 1<<62 {
		t.Errorf("runtime limit changed to %d with no limit anywhere", got)
	}
	if b.GoMemLimitFrom != "none" {
		t.Errorf("budget says %q, want none", b.GoMemLimitFrom)
	}
}

// keepGOMAXPROCS restores GOMAXPROCS after a test changes it. These tests
// do not run in parallel for the same reason: it is process-wide.
func keepGOMAXPROCS(t *testing.T, n int) {
	t.Helper()
	prev := runtime.GOMAXPROCS(n)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
}

// The pool follows GOMAXPROCS, not NumCPU.
//
// Sixteen is chosen because it is above this machine's NumCPU of four:
// pgx's own default would answer max(4, 4) = 4 here, so a pool that
// quietly went back to pgx's default fails this line. The deployment
// that motivated the change - GOMAXPROCS below a large NumCPU - cannot
// be built on a four-core machine, but a regression to NumCPU is caught
// from this side just the same.
func TestThePoolIsSizedFromGOMAXPROCS(t *testing.T) {
	for _, tc := range []struct {
		gomaxprocs int
		want       int32
	}{
		{1, 4}, // a 0.5-CPU container: the runtime says 2 at least, pgx's floor says 4
		{2, 4},
		{8, 8},
		{16, 16}, // above NumCPU on this machine; pgx's default would say 4
	} {
		keepGOMAXPROCS(t, tc.gomaxprocs)
		cfg, err := ParseConfig("postgres://u:p@127.0.0.1:5432/db?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MaxConns != tc.want {
			t.Errorf("GOMAXPROCS=%d: pool of %d, want %d", tc.gomaxprocs, cfg.MaxConns, tc.want)
		}
	}
}

// An operator who wrote pool_max_conns keeps it, in either spelling, and
// whatever the CPU budget says.
func TestTheOperatorsPoolSizeWins(t *testing.T) {
	keepGOMAXPROCS(t, 16)
	for _, dsn := range []string{
		"postgres://u:p@127.0.0.1:5432/db?sslmode=disable&pool_max_conns=7",
		"host=127.0.0.1 port=5432 user=u password=p dbname=db sslmode=disable pool_max_conns=7",
	} {
		cfg, err := ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MaxConns != 7 {
			t.Errorf("%s: pool of %d, want the operator's 7", dsn, cfg.MaxConns)
		}
	}
}

// A floor above the chosen ceiling is the operator's number; the ceiling
// rises to meet it rather than the pool refusing to open.
func TestAMinimumAboveTheDefaultRaisesTheCeiling(t *testing.T) {
	keepGOMAXPROCS(t, 2)
	cfg, err := ParseConfig("postgres://u:p@127.0.0.1:5432/db?pool_min_conns=20")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 20 {
		t.Errorf("pool of %d with pool_min_conns=20, want 20", cfg.MaxConns)
	}
}

// Open has to use the configuration it sized. A version that parsed the
// DSN and then called pgxpool.New with the string would hand back pgx's
// default and every other test here would still pass.
func TestOpenUsesTheSizedConfiguration(t *testing.T) {
	keepGOMAXPROCS(t, 16)
	// pgxpool connects lazily, so no database is needed to see the size.
	pool, err := Open(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 16 {
		t.Errorf("Open's pool has %d connections, want 16", got)
	}
}

func TestABadDSNIsAnError(t *testing.T) {
	if _, err := ParseConfig("postgres://u:p@127.0.0.1:5432/db?pool_max_conns=zero"); err == nil {
		t.Error("an unparseable pool_max_conns was accepted")
	}
}
