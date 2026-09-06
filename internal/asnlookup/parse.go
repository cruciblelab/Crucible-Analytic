package asnlookup

import (
	"encoding/csv"
	"io"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/textsafe"
)

// parseCountryCSV reads one sapics/ip-location-db "user-country" CSV file
// (ipv4 or ipv6 - same three-column shape either way) and returns every
// range it contains as a rangeEntry.
//
// Verified directly against real downloaded data (not just the documented
// format) before writing this: no header row, plain comma-separated,
// e.g.:
//
//	1.0.0.0,1.0.0.255,AU
//	1.0.4.0,1.0.7.255,AU
//
// A real encoding/csv reader is used rather than strings.Split(",") even
// though country codes themselves never need quoting - the sibling
// origin-asn dataset from the same project does quote fields containing
// commas (e.g. `"Cloudflare, Inc."`, see parseASNCSV below), and using
// the same real parser for both avoids a class of bug entirely rather
// than trusting this file's specific columns never will.
func parseCountryCSV(r io.Reader) ([]rangeEntry[string], error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // rows are checked for exactly 3 fields below; don't let a malformed row abort the whole file
	cr.ReuseRecord = true

	var out []rangeEntry[string]
	for {
		record, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A genuine CSV syntax error mid-file: stop rather than risk
			// misreading subsequent rows from a reader in an unspecified
			// state, but keep whatever was already parsed - a truncated
			// prefix beats nothing.
			break
		}
		if len(record) != 3 {
			continue
		}

		start, err := netip.ParseAddr(strings.TrimSpace(record[0]))
		if err != nil {
			continue
		}
		end, err := netip.ParseAddr(strings.TrimSpace(record[1]))
		if err != nil {
			continue
		}
		if start.Is4() != end.Is4() {
			continue // shouldn't happen in a real file; a defensive guard against a mixed-family row
		}

		country, ok := countryCode(record[2])
		if !ok {
			continue
		}

		out = append(out, rangeEntry[string]{start: start, end: end, value: country})
	}
	return out, nil
}

// parseASNCSV reads one sapics/ip-location-db "origin-asn" CSV file (ipv4
// or ipv6 - same four-column shape either way) and returns every range it
// contains as a rangeEntry.
//
// Verified directly against real downloaded data before writing this: no
// header row, e.g.:
//
//	1.0.0.0,1.0.0.255,13335,"Cloudflare, Inc."
//	1.0.4.0,1.0.7.255,38803,Gtelecom Pty Ltd
//	1.0.64.0,1.0.127.255,18144,"Enecom,Inc."
//
// Organization names routinely contain literal commas and are properly
// CSV-quoted when they do - confirmed from real data, not assumed - which
// is why this uses encoding/csv rather than strings.Split(",") like
// parseCountryCSV.
func parseASNCSV(r io.Reader) ([]rangeEntry[asnInfo], error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // rows are checked for exactly 4 fields below; don't let a malformed row abort the whole file
	cr.ReuseRecord = true

	var out []rangeEntry[asnInfo]
	for {
		record, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // same truncated-prefix-beats-nothing reasoning as parseCountryCSV
		}
		if len(record) != 4 {
			continue
		}

		start, err := netip.ParseAddr(strings.TrimSpace(record[0]))
		if err != nil {
			continue
		}
		end, err := netip.ParseAddr(strings.TrimSpace(record[1]))
		if err != nil {
			continue
		}
		if start.Is4() != end.Is4() {
			continue
		}

		asn, ok := asnNumber(record[2])
		if !ok {
			continue
		}

		org := orgName(record[3])
		if org == "" {
			continue
		}

		out = append(out, rangeEntry[asnInfo]{start: start, end: end, value: asnInfo{asn: asn, org: org}})
	}
	return out, nil
}

// What a value from these files is allowed to become.
//
// # Why there are checks here at all
//
// Both of these end up in `country`, `asn` and `asn_org` on every row
// the collector and the beacon write while the table is loaded. That is
// the difference between this file and the beacon's payload: a hostile
// event spoils its own row, and a hostile *dataset* row spoils every row
// attached to it, from both data sources, until the next refresh.
//
// PostgreSQL TEXT holds neither a NUL byte nor invalid UTF-8, and rows
// are written in batches - so an unwritable value here does not lose one
// visitor, it loses the batch.
//
// # How these were found
//
// By FuzzCountryCSVNeverProducesAnUnstorableValue and its ASN sibling,
// on their seed corpus, before the mutator had run once. Reading the
// code did not find them and the tests did not either, because both
// checked the strings somebody had thought of - and nobody thinks of
// "two NUL bytes" when the rule in their head is "two letters".

// maxOrgLen bounds an organisation name.
//
// The real ones run to a few dozen characters. The field had no bound at
// all, and the file is not ours: unbounded means whatever the next
// upstream release happens to contain, in a column on every row.
const maxOrgLen = 128

// countryCode returns the two-letter code a field holds, if it holds
// one.
//
// The check it replaces was `len(country) != 2`, which counts *bytes*.
// Two bytes is one 'Ü' and it is also two NUL bytes, and both got
// through - the first as a country nobody can group by, the second as a
// value PostgreSQL refuses along with everything batched beside it.
//
// Spelled out as two ASCII letters rather than as a rune count, because
// a rune count would still admit "Üé". The format's own definition is
// ISO 3166-1 alpha-2, and that is what this says.
func countryCode(field string) (string, bool) {
	c := strings.ToUpper(strings.TrimSpace(field))
	if len(c) != 2 {
		return "", false
	}
	for i := range 2 {
		if c[i] < 'A' || c[i] > 'Z' {
			return "", false
		}
	}
	return c, true
}

// orgName returns the storable, bounded form of an organisation name, or
// "" when nothing is left of it.
//
// textsafe.Storable rather than a copy of it: this was the third place
// in the repository that needed the rule, and the first that did not
// have it. See that package for why it is a package.
func orgName(field string) string {
	org := strings.TrimSpace(textsafe.Storable(field))
	if len(org) <= maxOrgLen { // bytes >= runes, so this is the cheap path
		return org
	}
	count := 0
	for i := range org {
		count++
		if count > maxOrgLen {
			return strings.TrimSpace(org[:i])
		}
	}
	return org
}

// asnNumber returns the AS number a field holds, if it holds one that
// this product can store.
//
// # Why the bound is here and not in the database
//
// `asn` is INTEGER in all three tables that carry it - ip_asn_ranges,
// beacon_events and traffic_snapshots - so the largest value that can be
// written is 2147483647. The check this replaces was `asn <= 0` after a
// strconv.Atoi, and Atoi's width is the platform's int: on a 64-bit
// machine 7000000000 parsed happily, went into a row, and failed the
// INSERT along with every row batched beside it. On a 32-bit machine the
// same line was skipped. An architecture-dependent answer to "is this
// dataset row usable" is not an answer.
//
// # The values this rejects that are real
//
// Four-byte AS numbers go to 4294967295, so 2147483648 upwards are
// legitimate numbers - the private-use range 4200000000-4294967294 among
// them. They are not globally routable and have no business in an
// origin-ASN dataset, but a leaked route could put one there.
//
// Such a range is dropped, and dropping it is the better of the two
// available answers: one missing range against every batch it would have
// been written into. Widening three columns to BIGINT for numbers that
// are not routable would be a schema change paid by every install.
func asnNumber(field string) (int, bool) {
	// ParseInt with an explicit width rather than Atoi: the question is
	// whether the number fits the column, and the column's width does
	// not change with the machine.
	n, err := strconv.ParseInt(strings.TrimSpace(field), 10, 32)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int(n), true
}
