package web

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// TestEveryPolicyHasASentenceOnTheRefusalPage.
//
// C8's second half: "neden giremiyorum" has to be answerable from the
// screen. A policy with no sentence renders an empty paragraph, which is
// indistinguishable from a deployment nobody can reach - the exact
// impression the requirement exists to prevent.
//
// Derived from the enum in the registry rather than from a list here, so
// a fourth policy added next year fails this on the day it is added.
// That is the same shape as the category test D4c needed, and it is here
// for the same reason: the failure of a missing entry is silence.
func TestEveryPolicyHasASentenceOnTheRefusalPage(t *testing.T) {
	def, ok := panel.DefinitionFor(panel.KeyDevAccessPolicy)
	if !ok {
		t.Fatal("the developer access policy is not in the settings registry")
	}
	if len(def.Enum) == 0 {
		t.Fatal("the policy setting has no enum, so this test compares nothing")
	}

	for _, mode := range def.Enum {
		key, ok := devAccessPolicyMessage[mode]
		if !ok {
			t.Errorf("policy %q has no sentence on the refusal page.\n"+
				"A developer refused under it would be shown an empty paragraph, "+
				"which reads as a broken deployment rather than as a decision "+
				"somebody made", mode)
			continue
		}
		if key == "" {
			t.Errorf("policy %q maps to an empty message key", mode)
		}
	}

	// And nothing the other way: a sentence for a policy that does not
	// exist is a sentence nobody will ever see, and the catalogue check
	// would go on believing it is used.
	inEnum := map[string]bool{}
	for _, mode := range def.Enum {
		inEnum[mode] = true
	}
	for mode := range devAccessPolicyMessage {
		if !inEnum[mode] {
			t.Errorf("the refusal page has a sentence for policy %q, which is not "+
				"one the setting accepts", mode)
		}
	}
}
