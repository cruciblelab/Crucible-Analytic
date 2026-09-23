// Package resources tells a service what it may actually use, and
// applies the one limit the Go runtime does not apply by itself.
//
// # What was measured before this was written (2026-09-23)
//
// PLAN §Z1 said a process in a 0.5-CPU container "cannot see its own
// budget", because runtime.NumCPU() still reports every core on the
// host. Half of that was wrong, and the wrong half would have produced
// a second copy of work the runtime already does. Asked of a probe built
// with this module's own go directive, inside real cgroup v1 limits:
//
//	limit                 NumCPU  GOMAXPROCS  memory limit seen
//	none                  4       4           none
//	CPU quota 2.0         4       2           none
//	CPU quota 1.0         4       2           none
//	CPU quota 0.5         4       2           none
//	memory 64 MB          4       4           none
//	64 MB + GOMEMLIMIT    4       4           40 MiB (the variable)
//
// So since Go 1.25 the runtime reads the CPU quota itself and sets
// GOMAXPROCS from it, with a floor of two. What it does not read is the
// memory limit. That draws this package's boundary:
//
//   - CPU: nothing is read here. GOMAXPROCS is already the container's
//     answer, and a second implementation of the runtime's logic would
//     be two things that can disagree. The only thing wrong with CPU is
//     a number somebody derives from NumCPU instead - see pool.go.
//   - Memory: the limit is found here and handed to the runtime as its
//     soft memory limit, unless an operator set GOMEMLIMIT.
//
// # No new setting, and why
//
// Both overrides already exist and are the ones an operator will look
// for: GOMEMLIMIT in the environment (including GOMEMLIMIT=off), and
// pool_max_conns in the DSN. A third spelling of either would be a
// second place to look and a question about which one wins.
package resources

import (
	"bufio"
	"bytes"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// memoryShare is how much of a memory limit the Go heap is told it may
// use. The rest is for what a cgroup counts and the runtime does not
// manage: the binary's own pages, the page cache charged to the group,
// and memory the runtime has asked the kernel for but not returned.
//
// The value is measured rather than borrowed - see NOTES, "Z1".
const memoryShare = 0.8

// unlimitedFloor is where a cgroup v1 limit stops meaning anything.
//
// v1 writes "no limit" as the largest page-aligned int64,
// 9223372036854771712 - read off a real hierarchy on 2026-09-23, where
// the leaf held 14345912320 and all three parents held that sentinel.
// Anything at or above 2^62 is treated the same way: no deployment has
// four exbibytes, and a limit that large constrains nothing.
const unlimitedFloor = int64(1) << 62

// MemoryLimit is the memory ceiling this process lives under.
type MemoryLimit struct {
	// Bytes is the ceiling, or zero when none was found.
	Bytes int64
	// Source says where it came from, for the log line: "cgroup v1",
	// "cgroup v2", or empty.
	Source string
}

// Budget is what Apply found and did. It is returned as well as logged,
// so the services that report their health can say it without asking
// the kernel a second time.
type Budget struct {
	NumCPU     int
	GOMAXPROCS int
	Memory     MemoryLimit
	// GoMemLimit is the runtime's soft memory limit after Apply, and
	// GoMemLimitFrom says who set it: "GOMEMLIMIT" (the operator),
	// "cgroup" (this package), or "none".
	GoMemLimit     int64
	GoMemLimitFrom string
	// PoolMaxConns is what a database pool gets when its DSN does not
	// say - see DefaultPoolMaxConns.
	PoolMaxConns int32
}

// Apply finds this process's memory ceiling, hands a share of it to the
// runtime unless the operator already decided, and logs one line saying
// what it saw.
//
// Every service calls it once, near the top of main, before it opens a
// pool or loads anything large. internal/invariants holds the list of
// services to that, and derives the list from the systemd units rather
// than from anybody's memory.
func Apply(logger *slog.Logger) Budget {
	return apply(logger, os.ReadFile, os.Getenv)
}

// reader and getenv are the two things apply takes from the outside,
// so the tests can hand it a hierarchy this machine does not have.
type reader func(string) ([]byte, error)

func apply(logger *slog.Logger, read reader, getenv func(string) string) Budget {
	b := Budget{
		NumCPU:       runtime.NumCPU(),
		GOMAXPROCS:   runtime.GOMAXPROCS(0),
		Memory:       detectMemory(read),
		PoolMaxConns: DefaultPoolMaxConns(),
	}

	switch {
	case getenv("GOMEMLIMIT") != "":
		// The operator decided, including "off". The runtime already
		// read the variable at start-up, so there is nothing to do but
		// report what it made of it.
		b.GoMemLimit = debug.SetMemoryLimit(-1)
		b.GoMemLimitFrom = "GOMEMLIMIT"
	case b.Memory.Bytes > 0:
		b.GoMemLimit = int64(float64(b.Memory.Bytes) * memoryShare)
		debug.SetMemoryLimit(b.GoMemLimit)
		b.GoMemLimitFrom = "cgroup"
	default:
		b.GoMemLimit = debug.SetMemoryLimit(-1)
		b.GoMemLimitFrom = "none"
	}

	if logger != nil {
		logger.Info("resources: budget",
			"gomaxprocs", b.GOMAXPROCS,
			"numcpu", b.NumCPU,
			"memory_limit", describeBytes(b.Memory.Bytes),
			"memory_limit_from", orDash(b.Memory.Source),
			"gomemlimit", describeBytes(b.GoMemLimit),
			"gomemlimit_from", b.GoMemLimitFrom,
			"pool_max_conns_default", b.PoolMaxConns,
		)
	}
	return b
}

func describeBytes(n int64) string {
	if n <= 0 || n == math.MaxInt64 {
		return "none"
	}
	return strconv.FormatInt(n, 10)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// detectMemory finds the smallest memory limit on the path from this
// process's cgroup up to the root of its hierarchy.
//
// The smallest, not the leaf's own: a limit on any ancestor constrains
// every group under it, and a leaf that reports "no limit" can still be
// killed by its parent's. The real hierarchy this was written against
// had the limit on the leaf and nothing above it; the tests hold the
// other arrangement too.
//
// v1 is asked first when it carries the memory controller. A host can
// have both - this one lists a "0::/" line in /proc/self/cgroup and has
// no cgroup2 filesystem mounted at all - and the version that holds the
// memory controller is the one whose limit the kernel enforces.
func detectMemory(read reader) MemoryLimit {
	groups, err := read("/proc/self/cgroup")
	if err != nil {
		return MemoryLimit{}
	}
	mounts, err := read("/proc/self/mountinfo")
	if err != nil {
		return MemoryLimit{}
	}

	v1Path, v2Path, hasV1, hasV2 := cgroupPaths(groups)

	if hasV1 {
		if root, point, ok := mountFor(mounts, "cgroup", "memory"); ok {
			if n := smallestLimit(read, point, within(v1Path, root, point),
				"memory.limit_in_bytes"); n > 0 {
				return MemoryLimit{Bytes: n, Source: "cgroup v1"}
			}
			// A v1 memory hierarchy with no limit anywhere is an answer:
			// the controller is here and it says unlimited.
			return MemoryLimit{}
		}
	}
	if hasV2 {
		if root, point, ok := mountFor(mounts, "cgroup2", ""); ok {
			if n := smallestLimit(read, point, within(v2Path, root, point),
				"memory.max"); n > 0 {
				return MemoryLimit{Bytes: n, Source: "cgroup v2"}
			}
		}
	}
	return MemoryLimit{}
}

// cgroupPaths reads /proc/self/cgroup: the v1 memory controller's path
// and the v2 unified path, either of which may be absent.
//
// Each line is "hierarchy-id:controllers:path". v2 is the line whose id
// is 0 and whose controller list is empty.
func cgroupPaths(b []byte) (v1Path, v2Path string, hasV1, hasV2 bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			v2Path, hasV2 = parts[2], true
			continue
		}
		for _, c := range strings.Split(parts[1], ",") {
			if c == "memory" {
				v1Path, hasV1 = parts[2], true
			}
		}
	}
	return
}

// mountFor finds a cgroup filesystem of the given type in mountinfo and
// returns the root it exposes and where it is mounted.
//
// For v1 the controller has to appear in the super options, because
// each v1 controller is its own mount. For v2 there is one mount and no
// controller to match.
//
// Line shape, from proc(5):
//
//	36 35 98:0 /root /mount rw,noatime shared:1 - cgroup cgroup rw,memory
//
// Fields before " - " are fixed position (root is the fourth, the mount
// point the fifth); after it come the filesystem type, the source, and
// the super options.
func mountFor(b []byte, fstype, controller string) (root, point string, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		pre, post, found := strings.Cut(sc.Text(), " - ")
		if !found {
			continue
		}
		pf := strings.Fields(pre)
		af := strings.Fields(post)
		if len(pf) < 5 || len(af) < 3 || af[0] != fstype {
			continue
		}
		if controller != "" {
			match := false
			for _, o := range strings.Split(af[2], ",") {
				if o == controller {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		return pf[3], pf[4], true
	}
	return "", "", false
}

// within turns a cgroup path into a directory under the mount point.
//
// Inside a cgroup namespace the path is already relative to the mount's
// root; outside one, the mount may expose a subtree, and the part of
// the path that the mount root already accounts for has to come off.
// Either way the answer never escapes the mount point.
func within(cgPath, root, point string) string {
	rel := cgPath
	if root != "/" && strings.HasPrefix(cgPath, root) {
		rel = strings.TrimPrefix(cgPath, root)
	}
	dir := filepath.Join(point, filepath.Clean("/"+rel))
	if !strings.HasPrefix(dir, point) {
		return point
	}
	return dir
}

// smallestLimit walks from dir up to the mount point and returns the
// smallest real limit it reads, or zero when every level is unlimited
// or unreadable.
func smallestLimit(read reader, point, dir, file string) int64 {
	var smallest int64
	for {
		if n, ok := parseLimit(read(filepath.Join(dir, file))); ok {
			if smallest == 0 || n < smallest {
				smallest = n
			}
		}
		if dir == point || len(dir) <= len(point) {
			return smallest
		}
		dir = filepath.Dir(dir)
	}
}

// parseLimit reads one limit file. "max" (v2) and the v1 sentinel both
// mean no limit; an unreadable or malformed file is no answer rather
// than a zero, because a zero-byte limit read as a real one would tell
// the runtime to collect garbage continuously.
func parseLimit(b []byte, err error) (int64, bool) {
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n >= unlimitedFloor {
		return 0, false
	}
	return n, true
}
