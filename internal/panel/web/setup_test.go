package web

import (
	"reflect"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
)

func TestWizardStepsAreDistinctAndOrdered(t *testing.T) {
	seen := map[string]bool{}
	for i, step := range wizardSteps {
		if step.ID == "" {
			t.Fatalf("step %d has no id", i)
		}
		if seen[step.ID] {
			t.Fatalf("step %q appears twice", step.ID)
		}
		seen[step.ID] = true
		if stepIndex(step.ID) != i {
			t.Errorf("stepIndex(%q) = %d, want %d", step.ID, stepIndex(step.ID), i)
		}
	}
	if stepIndex("boyle-bir-adim-yok") != -1 {
		t.Error("an unknown step resolved to a real index")
	}
	// The order of the last two is the product. The check must come
	// before handover, because handover is the step that decides the
	// deployment is fit to give somebody - and handover must be last,
	// because it is the only step that ends the installer's involvement:
	// everything before it can be revisited, and this one produces a
	// link that somebody else uses.
	if got := wizardSteps[len(wizardSteps)-1].ID; got != "devir" {
		t.Errorf("the wizard does not end on handover: %q", got)
	}
	if stepIndex("kontrol") >= stepIndex("devir") {
		t.Error("handover comes before the check that decides whether it may happen")
	}
}

// TestParseSiteListAcceptsWhatSomebodyWouldPaste. An installer copies
// this from a config file (one per line) or from a note (comma
// separated), and being told the format was wrong is pure friction when
// both are unambiguous.
func TestParseSiteListAcceptsWhatSomebodyWouldPaste(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"blog\nshop", []string{"blog", "shop"}},
		{"blog, shop", []string{"blog", "shop"}},
		{" blog \n\n shop \r\n", []string{"blog", "shop"}},
		{"blog\tshop", []string{"blog", "shop"}},
		// A duplicate is a paste accident, not an instruction.
		{"blog\nblog\nshop", []string{"blog", "shop"}},
		{"", nil},
		{"   \n  ", nil},
	}
	for _, tc := range cases {
		got := parseSiteList(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseSiteList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseSiteListDoesNotValidate is worth pinning: the character
// rules live in the settings registry, which refuses a bad id with a
// message naming it. Validating here as well would mean two places to
// keep in step, and the weaker one would eventually disagree.
func TestParseSiteListDoesNotValidate(t *testing.T) {
	got := parseSiteList("bir site/adı")
	if len(got) == 0 {
		t.Fatal("the parser silently dropped a value; the registry should be the one to refuse it")
	}
}

func TestDatabaseChecksNarrowToTheDatabase(t *testing.T) {
	all := []preflight.CheckResult{
		{ID: "schema.panel"},
		{ID: "grants.panel_isolation"},
		{ID: "roles.exist"},
		{ID: "retention.policies"},
		{ID: "log.dir"},
		{ID: "disk.free"},
		{ID: "service.collector"},
		{ID: "config.developer_password"},
	}
	got := databaseChecks(all)
	want := []string{"schema.panel", "grants.panel_isolation", "roles.exist", "retention.policies"}
	if len(got) != len(want) {
		t.Fatalf("got %d checks, want %d: %+v", len(got), len(want), got)
	}
	for i, check := range got {
		if check.ID != want[i] {
			t.Errorf("check %d = %q, want %q", i, check.ID, want[i])
		}
	}
}

func TestToStringList(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{
		{[]string{"a", "b"}, []string{"a", "b"}},
		{[]any{"a", "b"}, []string{"a", "b"}},
		{"a", []string{"a"}},
		{"", nil},
		{nil, nil},
		{42, nil},
	}
	for _, tc := range cases {
		got := toStringList(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toStringList(%#v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRetentionKeysAreAllGuarded(t *testing.T) {
	// If one of these ever stops needing the developer password, the
	// wizard step's whole shape - a password field and a per-key
	// authorization - becomes wrong. Better to find out here.
	if !panel.NeedsDeveloperPassword(retentionKeys...) {
		t.Fatal("the retention keys are no longer guarded; the wizard step assumes they are")
	}
}
