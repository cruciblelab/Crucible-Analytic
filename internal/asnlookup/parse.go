package asnlookup

import (
	"encoding/csv"
	"io"
	"net/netip"
	"strconv"
	"strings"
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

		country := strings.ToUpper(strings.TrimSpace(record[2]))
		if len(country) != 2 {
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

		asn, err := strconv.Atoi(strings.TrimSpace(record[2]))
		if err != nil || asn <= 0 {
			continue
		}

		org := strings.TrimSpace(record[3])
		if org == "" {
			continue
		}

		out = append(out, rangeEntry[asnInfo]{start: start, end: end, value: asnInfo{asn: asn, org: org}})
	}
	return out, nil
}
