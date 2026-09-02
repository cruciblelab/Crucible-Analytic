// Package memlimit answers one question: how much memory may this
// process actually use?
//
// # Why it exists
//
// A3 measured what the IP-intelligence datasets cost - 59 MB held in
// country-only mode, 136 MB with the ASN half, and a parse peak of
// roughly one and a half to two times either figure. A2 turns those into
// a choice an operator makes.
//
// A choice made blind is not a choice. Somebody running the stack in a
// container with `mem_limit: 256m` who picks the largest profile does not
// get a warning and a degraded service; they get the collector killed by
// the kernel, and the collector stands in front of the customer's site,
// so the site goes with it. The failure arrives as "the site is down"
// and the cause is a dropdown three weeks earlier.
//
// So the profile is checked against a ceiling before it is accepted, and
// this package is where the ceiling comes from.
//
// # What it will not do
//
// It does not decide anything. It reports a number and where the number
// came from, because those two facts have different weights: a container
// limit is exact and belongs to this process alone, while free memory on
// a shared machine is an estimate that the database next door can
// invalidate a minute later. A caller that treated them alike would be
// as confident about a guess as about a measurement.
package memlimit

import (
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Source says where a ceiling came from. Reported alongside the number
// because the caller's confidence should differ.
type Source string

const (
	// SourceCgroupV2 is /sys/fs/cgroup/memory.max: a hard ceiling the
	// kernel enforces on this process, set by `docker run --memory`,
	// compose's mem_limit, or a systemd MemoryMax=.
	SourceCgroupV2 Source = "cgroup v2"
	// SourceCgroupV1 is the same thing one kernel generation back, at
	// /sys/fs/cgroup/memory/memory.limit_in_bytes. Still what Docker
	// writes on a host whose cgroup tree is v1 - measured, on a v1 host:
	// `--memory=256m` produces exactly 268435456 there and nothing at
	// all under the v2 path.
	SourceCgroupV1 Source = "cgroup v1"
	// SourceAvailable is MemAvailable from /proc/meminfo: the kernel's
	// own estimate of what a new allocation could obtain right now,
	// without swapping.
	//
	// MemAvailable rather than MemTotal, and the difference is the whole
	// point on the deployment this product actually ships to. A single
	// VDS runs the collector, the beacon, the API, the panel and
	// TimescaleDB together, and TimescaleDB is sized to take most of the
	// machine - timescaledb-tune sets shared_buffers to about a quarter
	// of RAM before anything else. MemTotal would report a number no
	// service can have.
	SourceAvailable Source = "free memory"
	// SourceUnknown means nothing could be read. Every file below is
	// Linux-specific and any of them can be absent - an unprivileged
	// container with a masked /proc, a non-Linux developer machine.
	SourceUnknown Source = "unknown"
)

// Limit is a ceiling and its provenance. Bytes is zero when From is
// SourceUnknown, and a caller must treat that as "do not know" rather
// than as "no memory": refusing a profile because a file could not be
// read would turn an unreadable /proc into an outage.
type Limit struct {
	Bytes uint64
	From  Source
}

// Known reports whether a ceiling was actually found.
func (l Limit) Known() bool { return l.From != SourceUnknown && l.Bytes > 0 }

// Exact reports whether this ceiling is enforced rather than estimated.
//
// The distinction the package doc promises, in the form a caller can
// act on. A cgroup limit is a number the kernel will kill this process
// for exceeding: it does not move, it belongs to this process alone, and
// a profile that does not fit under it will die - not might.
//
// MemAvailable is none of those things. It is what the machine happens
// to have free at the instant it was read, on a box where TimescaleDB is
// sized to take most of the memory and where a backup or a query can
// move the number by hundreds of megabytes between two readings.
//
// So a caller that refuses to start on an exact ceiling is refusing
// something that was going to fail anyway; a caller that refuses on an
// estimate has turned a busy minute into an outage. The first is worth
// doing and the second is the thing this method exists to stop.
func (l Limit) Exact() bool {
	return l.From == SourceCgroupV2 || l.From == SourceCgroupV1
}

// Detect reads this process's memory ceiling.
func Detect() Limit { return detect(os.DirFS("/")) }

// detect is Detect over a filesystem, so the tests can build one instead
// of describing it.
//
// # An fs.FS rather than a path prefix
//
// It began as detect(root string) with os.ReadFile(root + path), which
// is the obvious shape and which gosec rejects: G304, "potential file
// inclusion via variable", because the argument is not a constant. The
// merge gate caught it - correctly, and after this package had been
// pushed with a green `go test ./...` behind it, which runs the tests
// and not the gate.
//
// It could have been answered with a baseline entry saying the paths are
// all literals. This is better: with fs.ReadFile the names *are*
// literals, at every call, and no future caller can pass a path from
// somewhere else because there is nowhere else to pass one from. A
// finding removed beats a finding explained, and the explanation would
// have had to stay true forever.
//
// The order is most specific first. A container carries both a cgroup
// limit and a MemAvailable, and the cgroup limit is the one that kills
// it; reading them the other way round would report the host's free
// memory to a process that may not have any of it.
func detect(fsys fs.FS) Limit {
	// The machine's total, used below to recognise a cgroup "limit" that
	// is not one. Read first because both cgroup branches need it.
	total := meminfoValue(fsys, "MemTotal")

	if v, ok := readUint(fsys, cgroupV2Path); ok && isRealLimit(v, total) {
		return Limit{Bytes: v, From: SourceCgroupV2}
	}
	if v, ok := readUint(fsys, cgroupV1Path); ok && isRealLimit(v, total) {
		return Limit{Bytes: v, From: SourceCgroupV1}
	}
	if v := meminfoValue(fsys, "MemAvailable"); v > 0 {
		return Limit{Bytes: v, From: SourceAvailable}
	}
	return Limit{From: SourceUnknown}
}

// The three files, as constants. Unrooted, because that is what an
// fs.FS takes; Detect supplies the root.
const (
	cgroupV2Path = "sys/fs/cgroup/memory.max"
	cgroupV1Path = "sys/fs/cgroup/memory/memory.limit_in_bytes"
	meminfoPath  = "proc/meminfo"
)

// isRealLimit rejects the value an unlimited cgroup reports.
//
// Derived rather than listed, which matters because the sentinel is not
// one value. cgroup v2 writes the literal "max" (readUint rejects it as
// unparseable). cgroup v1 writes a huge number instead, and *which* huge
// number depends on the page size: it is the largest multiple of the
// page size that fits in an int64, so 9223372036854771712 on a 4 KiB
// system and something else elsewhere. Measured here as exactly that on
// an unlimited process, alongside 268435456 for --memory=256m.
//
// Matching either constant would be a list that goes stale on a kernel
// nobody has yet. The property that holds regardless: a ceiling larger
// than the machine it runs on is not a ceiling. Anything at or above
// MemTotal is treated as absent, and the caller falls through to what
// is actually free.
//
// When MemTotal is unreadable there is nothing to compare against, so
// the value is taken at face value: a wrong ceiling from a real cgroup
// file is still closer than no ceiling at all.
func isRealLimit(v, total uint64) bool {
	if v == 0 {
		return false
	}
	if total == 0 {
		return true
	}
	return v < total
}

// readUint reads a file holding a single decimal number.
func readUint(fsys fs.FS, path string) (uint64, bool) {
	body, err := fs.ReadFile(fsys, path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		// "max", on cgroup v2, and anything else unexpected. Not an
		// error worth reporting: it means this file does not carry a
		// limit, which is a normal state.
		return 0, false
	}
	return v, true
}

// meminfoValue reads one kB-denominated field out of /proc/meminfo and
// returns it in bytes. Zero when absent.
func meminfoValue(fsys fs.FS, field string) uint64 {
	body, err := fs.ReadFile(fsys, meminfoPath)
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != field {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		// Every size in /proc/meminfo is in kB, and the unit is spelled
		// out in the line. Checked rather than assumed: a field that
		// ever stopped carrying it would otherwise be read as bytes and
		// come out a thousand times too small, which is the direction
		// that silently refuses every profile.
		if len(fields) > 1 && !strings.EqualFold(fields[1], "kB") {
			return 0
		}
		return v * 1024
	}
	return 0
}
