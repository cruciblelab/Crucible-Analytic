// Package docs holds no code. It exists so that the invariants this
// project's documents have to satisfy are checked by `go test` rather
// than by somebody remembering to look.
//
// There are two, and both have been broken in this repository before:
//
//   - Turkish text must not be mojibake. KURULUM.md, PLAN.md and NOTES.md
//     are written in Turkish, and a document whose "ş" has become "Å" is
//     a document the reader stops trusting. It was checked by hand after
//     every edit until this package existed.
//   - A reference to another document must resolve. KURULUM.md sent
//     readers to a file that had never existed in this repository, and
//     nothing noticed until the references were checked deliberately.
package docs
