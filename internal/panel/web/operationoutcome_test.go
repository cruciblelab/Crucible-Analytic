package web

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// panelErrors is every exported error the panel package declares, by
// name, with what an operation record should call it.
//
// The list of names is derived from the source (panelErrorNames); this
// table only classifies. A sentinel added to the panel without a line
// here fails TestEveryRefusalThePanelNamesIsRecordedAsOne by name - the
// decision "is this a refusal" has to be made once, where it is made for
// every other one, and not discovered from an operation list that calls
// a wrong password a failure. That is how it was found (2026-11-02):
// outcomeFor knew the settings page's five refusals, five more forms
// called it, and none of their refusals were on it.
var panelErrors = map[string]struct {
	err error
	// refusal says the error is the system declining: recorded as
	// OutcomeRefused. Every other entry carries why it is not on the
	// list - none of them is returned by a form that records an
	// operation.
	refusal bool
	why     string
}{
	// Refusals of a form that records an operation.
	"ErrSettingNotWritable":        {panel.ErrSettingNotWritable, true, ""},
	"ErrDeveloperPasswordRequired": {panel.ErrDeveloperPasswordRequired, true, ""},
	"ErrUnknownSetting":            {panel.ErrUnknownSetting, true, ""},
	"ErrPreconditionUnmet":         {panel.ErrPreconditionUnmet, true, ""},
	"ErrInvalidSetting":            {panel.ErrInvalidSetting, true, ""},
	"ErrUpgradeLocked":             {panel.ErrUpgradeLocked, true, ""},
	"ErrUpgradeNotNeeded":          {panel.ErrUpgradeNotNeeded, true, ""},
	"ErrReleaseLocked":             {panel.ErrReleaseLocked, true, ""},
	"ErrReleaseBadVersion":         {panel.ErrReleaseBadVersion, true, ""},
	"ErrSecretsPasswordRequired":   {panel.ErrSecretsPasswordRequired, true, ""},
	"ErrBackupInFlight":            {panel.ErrBackupInFlight, true, ""},
	"ErrRangeRefreshBusy":          {panel.ErrRangeRefreshBusy, true, ""},
	"ErrRestartNeedsPassword":      {panel.ErrRestartNeedsPassword, true, ""},
	"ErrRestartUnknownService":     {panel.ErrRestartUnknownService, true, ""},
	"ErrRestartCannotHear":         {panel.ErrRestartCannotHear, true, ""},
	"ErrRestartNeverReported":      {panel.ErrRestartNeverReported, true, ""},
	"ErrRestartTooSoon":            {panel.ErrRestartTooSoon, true, ""},

	// Returned where no operation is recorded.
	"ErrNoMailAccount":         {panel.ErrNoMailAccount, false, "mail settings"},
	"ErrTooManyInvites":        {panel.ErrTooManyInvites, false, "members page"},
	"ErrInviteInvalid":         {panel.ErrInviteInvalid, false, "accepting an invitation"},
	"ErrOwnershipCannotExpire": {panel.ErrOwnershipCannotExpire, false, "members page"},
	"ErrLastOwner":             {panel.ErrLastOwner, false, "members page"},
	"ErrNotPermitted":          {panel.ErrNotPermitted, false, "members page"},
	"ErrUnknownService":        {panel.ErrUnknownService, false, "migration of a service's settings at start"},
	"ErrClaimInvalid":          {panel.ErrClaimInvalid, false, "the owner's first claim"},
	"ErrRecoveryInvalid":       {panel.ErrRecoveryInvalid, false, "signing in with a recovery code"},
	"ErrNoSession":             {panel.ErrNoSession, false, "sessions"},
	"ErrNotFound":              {panel.ErrNotFound, false, "a lookup"},
	"ErrEmailTaken":            {panel.ErrEmailTaken, false, "creating an account"},
	"ErrEmailUnusable":         {panel.ErrEmailUnusable, false, "creating an account"},
	"ErrTOTPReplayed":          {panel.ErrTOTPReplayed, false, "signing in"},
	"ErrTOTPInvalid":           {panel.ErrTOTPInvalid, false, "signing in"},
	"ErrDevAccessDecided":      {panel.ErrDevAccessDecided, false, "the owner's answer to a developer session"},
	"ErrDevAccessInvalid":      {panel.ErrDevAccessInvalid, false, "the owner's answer to a developer session"},
	"ErrIssueNeedsOperator":    {panel.ErrIssueNeedsOperator, false, "issuing a recovery code"},
	"ErrIssueNeedsPassword":    {panel.ErrIssueNeedsPassword, false, "issuing a recovery code"},
	"ErrLiftedRecently":        {panel.ErrLiftedRecently, false, "lifting a sign-in restriction"},
	"ErrMayNotLift":            {panel.ErrMayNotLift, false, "lifting a sign-in restriction"},
	"ErrNotLocked":             {panel.ErrNotLocked, false, "lifting a sign-in restriction"},
	"ErrPasswordTooLong":       {panel.ErrPasswordTooLong, false, "choosing a password"},
	"ErrPasswordTooShort":      {panel.ErrPasswordTooShort, false, "choosing a password"},
	"ErrReasonTooLong":         {panel.ErrReasonTooLong, false, "a developer session's reason"},
}

// panelErrorNames reads every exported package-level Err variable the
// panel package declares, from its source.
func panelErrorNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if strings.HasPrefix(name.Name, "Err") && name.IsExported() {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// TestEveryRefusalThePanelNamesIsRecordedAsOne.
func TestEveryRefusalThePanelNamesIsRecordedAsOne(t *testing.T) {
	names := panelErrorNames(t)
	if len(names) < 30 {
		t.Fatalf("read only %d Err variables from internal/panel; the reader is not "+
			"finding the declarations, and a check over an empty list passes", len(names))
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
		c, ok := panelErrors[name]
		if !ok {
			t.Errorf("panel.%s has no line in panelErrors. Decide whether it is a refusal: "+
				"if a form that records an operation can return it, it belongs in "+
				"operationRefusals too", name)
			continue
		}
		// Wrapped, the way the stores return most of them.
		got := outcomeFor(fmt.Errorf("panel: something: %w", c.err))
		switch {
		case c.refusal && got != panel.OutcomeRefused:
			t.Errorf("panel.%s is a refusal and an operation would record it as %q", name, got)
		case !c.refusal && c.why == "":
			t.Errorf("panel.%s is not a refusal and does not say why it is not", name)
		}
	}
	for name := range panelErrors {
		if !seen[name] {
			t.Errorf("panelErrors has %s and the panel declares no such error", name)
		}
	}
}

// TestAFaultIsStillAFailure is the other half: a classifier that called
// everything a refusal would pass the test above.
func TestAFaultIsStillAFailure(t *testing.T) {
	for _, err := range []error{
		errors.New("panel: request release: connection refused"),
		fmt.Errorf("panel: %w", errors.New("deadline exceeded")),
	} {
		if got := outcomeFor(err); got != panel.OutcomeFailed {
			t.Errorf("%v is recorded as %q; a fault is a failure", err, got)
		}
	}
}

// TestTheBackupFormsMixedRequestIsARefusal: the one refusal on the list
// from another package, so not covered by the derived names.
func TestTheBackupFormsMixedRequestIsARefusal(t *testing.T) {
	if got := outcomeFor(fmt.Errorf("panel: backup: %w", backup.ErrMixedRequest)); got != panel.OutcomeRefused {
		t.Errorf("ticking the configuration and the data together is recorded as %q", got)
	}
}
