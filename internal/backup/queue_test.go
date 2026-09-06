package backup

import (
	"strings"
	"testing"
)

// What the answering side does with a row it does not understand.
//
// # Why this is a unit test and not an integration one
//
// Because the row cannot be inserted. panel_backup_requests has a CHECK
// naming the kinds this build knows, so a database this build set up
// can hold nothing else - and an integration test would have to widen
// the constraint first, which would be testing a database this product
// never ships.
//
// The case is still real, and it is the one this whole function exists
// for: a panel of a *later* version, whose schema allows a third kind,
// writing a row that an upgrader of an *earlier* version then claims.
// The two halves of a deployment are separate binaries and are not
// upgraded in the same instant. F1f's next phase adds exactly such a
// kind, so this is not hypothetical - it is the shape of the very next
// change.
//
// Measured rather than assumed: with the default branch returning nil,
// every other test in this package stayed green.
func TestARequestNamingWorkThisBuildDoesNotKnowIsRefused(t *testing.T) {
	for name, work := range map[string]Work{
		"a kind from a later version": "geri_yukle",
		"a kind from nowhere":         "sil",
		"no kind at all":              "",
	} {
		t.Run(name, func(t *testing.T) {
			id := int64(1)
			err := validateRequest(&Request{Work: work, TargetID: &id})
			if err == nil {
				t.Fatalf("work %q was accepted, so an upgrader would go on to do "+
					"whichever branch it fell through to", work)
			}
			if !strings.Contains(err.Error(), string(work)) && work != "" {
				t.Errorf("the refusal does not name the work: %v", err)
			}
		})
	}
}

// And the two it does know are accepted, or the check above would be
// satisfied by a function that refuses everything.
func TestTheTwoKindsThisBuildKnowsAreAccepted(t *testing.T) {
	id := int64(1)
	if err := validateRequest(&Request{Work: WorkTake, Sets: []string{SetPanel}}); err != nil {
		t.Errorf("a take was refused: %v", err)
	}
	if err := validateRequest(&Request{Work: WorkVerify, TargetID: &id}); err != nil {
		t.Errorf("a check was refused: %v", err)
	}
	// A take still has its sets checked, which is the other half of what
	// this function is for.
	if err := validateRequest(&Request{Work: WorkTake, Sets: []string{"analitik-eski"}}); err == nil {
		t.Error("a take naming a set this build does not know was accepted")
	}
}
