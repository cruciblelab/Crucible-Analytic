package heartbeat

import _ "embed"

// SchemaSQL is this package's schema, carried in the binary.
//
// Embedded rather than read from disk so the component that applies it
// is self-contained: the fingerprint in internal/schemaver names these
// exact bytes, and a check against a file somebody could have replaced
// would prove less than it appears to.
//
//go:embed schema.sql
var SchemaSQL string
