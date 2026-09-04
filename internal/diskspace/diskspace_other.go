//go:build !linux

package diskspace

import "fmt"

// read has no portable implementation, so on anything but Linux this
// reports "could not measure" rather than a number.
//
// The file exists so the package compiles everywhere developers work,
// not only where it is deployed - and CI cross-builds for darwin and
// windows, so a Linux-only package would turn that gate red.
//
// An error rather than a zero Space, for the same reason a missing
// directory is an error: this project keeps "we looked and it was fine"
// apart from "we did not look" everywhere else, and a Space full of
// zeroes is a disk with no room on it.
func read(path string) (Space, error) {
	return Space{}, fmt.Errorf("diskspace: %s: disk space cannot be measured on this "+
		"platform; this product is deployed on Linux", path)
}
