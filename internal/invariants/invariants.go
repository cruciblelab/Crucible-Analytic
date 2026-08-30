// Package invariants holds the structural rules this repository keeps,
// and nothing else.
//
// It has no production code and is imported by nothing. What lives here
// are tests that read the source tree and assert properties no single
// file can assert about itself - the kind of rule that is true because
// four packages each remembered it, and stops being true the day a
// fifth one does not.
//
// # Why this package exists
//
// Two holes found on the same afternoon, and they had one shape:
// *three of four servers did the right thing and one did not.*
//
//   - internal/api, internal/beacon and internal/panel/web all set their
//     four timeouts. internal/fullproxy set none.
//   - Everything running on net/http got a per-connection recover for
//     free. internal/proxy - the one package that does not use
//     http.Server - did not, so one panic on one connection took the
//     whole collector down, and with it the customer's website.
//
// A person found both. This package is the attempt not to depend on
// that person being there next time.
//
// # The rule these tests follow
//
// PLAN.md §3.4: *listeye karşı bakılır, hafızaya karşı değil* - checked
// against a list, not against memory. Every test here is a two-way
// mirror: one side is what the source actually does, read by walking
// its syntax tree; the other is a list written by hand, with a reason
// beside each entry. Either side changing without the other fails, and
// the failure names which side moved.
//
// That is deliberately more annoying than a one-way scan. A scan that
// only checked "every server it can find has a timeout" would pass
// silently the day somebody builds a server in a shape the scan does
// not recognise. The list is what makes a person look.
package invariants
