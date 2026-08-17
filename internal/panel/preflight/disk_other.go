//go:build !linux

package preflight

import "errors"

// freeBytes has no portable implementation, so on anything but Linux the
// disk check reports "could not measure" rather than a number.
//
// This file exists because the package must compile everywhere developers
// work, not only where it is deployed. Reporting an honest "not measured"
// on a Mac is better than a build failure, and better than a guess: this
// project keeps "we looked and it was fine" apart from "we did not look"
// everywhere else too.
func freeBytes(path string) (uint64, error) {
	if path == "" {
		return 0, errNoPath
	}
	return 0, errors.New("bu platformda disk alanı ölçülemiyor")
}
