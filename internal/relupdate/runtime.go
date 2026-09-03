package relupdate

import "runtime"

// The platform this build is for, in variables so a test can name a
// package that exists rather than one for whatever machine it runs on.
//
// Read through Source.goos/goarch rather than used directly, so the
// override is a field on a struct a caller passes in - not a global a
// test mutates and another test then reads.
var (
	runtimeGOOS   = runtime.GOOS
	runtimeGOARCH = runtime.GOARCH
)
