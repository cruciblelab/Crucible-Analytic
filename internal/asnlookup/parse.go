package asnlookup

import (
	"bufio"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// parseDelegatedStats reads one RIR "delegated-extended" stats file (the
// same public, standardized format all five RIRs publish) and returns
// every IPv4 allocation/assignment record found in it as a rangeEntry.
//
// The format is pipe-delimited, one record per line:
//
//	registry|cc|type|start|value|date|status[|opaque-id[|extensions...]]
//
// Two other line shapes appear in the same file and are deliberately
// skipped, both by the same two checks below rather than by pattern-
// matching each shape individually:
//   - The header line (version|registry|serial|records|startdate|enddate|
//     UTCoffset) has 7 fields like a real record, but its 3rd field is a
//     serial number, never the literal "ipv4".
//   - Per-resource-type summary lines (registry|*|type|*|count|summary)
//     have only 6 fields - one short of a real record - because they omit
//     the status column entirely.
//
// ASN (type "asn") and IPv6 (type "ipv6") records are skipped too - only
// IPv4 is in scope this phase (see the package doc comment for why ASN
// numbers aren't resolved at all yet).
func parseDelegatedStats(r io.Reader) ([]rangeEntry, error) {
	var out []rangeEntry
	scanner := bufio.NewScanner(r)
	// Default 64KiB max token size is more than enough for one pipe-
	// delimited record, but be explicit rather than rely on the default.
	scanner.Buffer(make([]byte, 0, 4096), 1<<16)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 7 {
			continue // header (see above) or a summary line
		}
		if parts[2] != "ipv4" {
			continue // header, "asn", or "ipv6" record
		}

		cc := strings.ToUpper(strings.TrimSpace(parts[1]))
		if len(cc) != 2 || cc == "ZZ" {
			continue // no usable (or explicitly "not disclosed") country
		}

		startAddr, err := netip.ParseAddr(parts[3])
		if err != nil || !startAddr.Is4() {
			continue
		}
		startInt, ok := ipv4ToUint32(startAddr)
		if !ok {
			continue
		}

		count, err := strconv.ParseUint(parts[4], 10, 32)
		if err != nil || count == 0 {
			continue
		}
		// RIR-delegated blocks near the top of the address space (e.g.
		// 255.x.x.x special-use ranges occasionally listed as reserved)
		// could in principle overflow uint32 when start+count-1 is
		// computed; skip rather than wrap silently into a bogus range.
		end := uint64(startInt) + count - 1
		if end > 0xFFFFFFFF {
			continue
		}

		out = append(out, rangeEntry{
			start:   startInt,
			end:     uint32(end),
			country: cc,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
