package panel

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// This file is the whole of what a call site has to know about the
// developer password. Three functions:
//
//	req    := store.GateRequest(principal, r, panel.KeyPrivacyIPStorage)
//	result := gate.Verify(ctx, req)
//	err    := store.ApplySetting(ctx, principal, key, site, value, result.For(...))
//
// Everything else - hashing, throttling, expiry, the audit row, the
// Turkish message - happens without the call site arranging it. That is
// deliberate: a guard that each caller has to remember to assemble
// correctly is a guard that will eventually be assembled incorrectly,
// and the failure will be silent.

// GateRequest builds the verification request for a set of guarded
// settings.
//
// The keys are the caller's own conclusion about what it is about to
// change - worked out from the form it just validated, never read from
// the request. A request that could name the settings it wanted
// authorized would be a request that authorizes itself.
//
// A principal who may not attempt the password at all produces a request
// with no actions, which the gate refuses without hashing anything. That
// matters more than it looks: the failure counter is shared, so a
// customer typing guesses into a form they should never have seen would
// otherwise lock the operator out of their own deployment. Refusing on
// identity, before the work, is what stops a security control from
// becoming a denial of service.
func (s *Store) GateRequest(a Access, r *http.Request, keys ...Key) devgate.Request {
	actions := make([]string, 0, len(keys))
	if a.MayAttemptDeveloperPassword() {
		for _, key := range keys {
			if def, ok := registry[key]; ok && def.RequiresDeveloperPassword {
				actions = append(actions, GateAction(key))
			}
		}
	}
	return devgate.Request{
		Actions:   actions,
		Password:  devgate.FromRequest(r),
		Actor:     a.Principal.Label,
		ActorKind: string(a.Principal.Kind),
		ActorID:   a.Principal.UserID,
		Peer:      devgate.PeerOf(r),
	}
}

// NeedsDeveloperPassword reports whether any of these settings is
// guarded, so a handler can decide whether to put the prompt up at all.
func NeedsDeveloperPassword(keys ...Key) bool {
	for _, key := range keys {
		if def, ok := registry[key]; ok && def.RequiresDeveloperPassword {
			return true
		}
	}
	return false
}

// GateAudit returns the hook to hand devgate.Options.Audit, so every
// developer password attempt lands in the append-only audit log.
//
// The table grants the panel role INSERT and SELECT but not UPDATE or
// DELETE, so a compromised panel process cannot erase the record of what
// it tried - which is the property that makes recording failed attempts
// worth anything.
func (s *Store) GateAudit() func(context.Context, devgate.Attempt) error {
	return func(ctx context.Context, a devgate.Attempt) error {
		action := ActionDevPasswordRefused
		if a.OK() {
			action = ActionDevPasswordGranted
		}

		kind := PrincipalKind(a.ActorKind)
		if kind == "" {
			// Never leave the actor kind blank: an entry that does not
			// say who acted is worse than no entry, because it reads as
			// if somebody checked.
			kind = PrincipalSystem
		}

		var actorID *int64
		if a.ActorID != 0 {
			id := a.ActorID
			actorID = &id
		}
		var ip *netip.Addr
		if addr, err := netip.ParseAddr(a.Peer); err == nil {
			ip = &addr
		}

		return s.Record(ctx, AuditEntry{
			ActorKind:  kind,
			ActorID:    actorID,
			ActorLabel: a.Actor,
			Action:     action,
			Target:     strings.Join(a.Actions, ","),
			Detail:     map[string]any{"decision": string(a.Decision)},
			IP:         ip,
		})
	}
}

// ApplySetting writes a setting and records who changed it, in one call.
//
// The audit entry carries the old value as well as the new one. "Who set
// the retention to 3650 days" is a question asked months later, by which
// point the only thing anybody remembers is that it used to be
// something else; an entry that records only the new value cannot answer
// it.
//
// For an unguarded setting the authorization is ignored, so a form that
// saves a mixed set of settings needs one loop rather than two - and
// therefore cannot send the guarded ones down the unguarded path by
// mistake.
// op may be nil. When it is not, the audit entry this writes is linked
// to it - done here rather than by the caller because this is the only
// place the entry's id exists, and an id passed back up through two
// signatures to be handed straight down again is a worse shape than a
// parameter that says what it is for.
func (s *Store) ApplySetting(ctx context.Context, a Access, key Key, site string, value any, auth devgate.Authorization, op *Operation) error {
	def, ok := registry[key]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	// Entitlement before authorization, and before anything that costs
	// work. A customer is refused here on who they are, whatever they
	// typed - so no password they could supply changes the answer, and
	// no attempt of theirs consumes the operator's failure budget.
	if !a.AccessTo(def).Editable() {
		return fmt.Errorf("%w (%s)", ErrSettingNotWritable, key)
	}
	// Checked before the write and before the audit entry, so a value
	// the deployment cannot honour never gets recorded as if it had
	// been applied.
	if err := s.checkPrecondition(key, value); err != nil {
		return err
	}

	before, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return err
	}

	if err := s.SetGuardedSetting(ctx, key, site, value, actorIDOf(a.Principal), auth); err != nil {
		return err
	}

	// Recorded after the write succeeded, so the log never claims a
	// change that did not happen. The reverse ordering - record, then
	// write - would produce an audit trail that is wrong in the
	// direction nobody checks.
	auditID, err := s.recordForReturningID(ctx, a.Principal, AuditEntry{
		Action: ActionSettingChanged,
		SiteID: site,
		Target: string(key),
		Detail: map[string]any{
			"from":    before,
			"to":      value,
			"guarded": def.RequiresDeveloperPassword,
		},
	})
	if err != nil {
		return err
	}
	op.LinkAudit(ctx, auditID)
	return nil
}

// ClearSetting removes a stored value so the default applies again, and
// records it.
//
// Guarded exactly like a write, because it is one: for
// campaign.drop_params the default is the empty list, so clearing it
// means starting to store utm_term again.
func (s *Store) ClearSetting(ctx context.Context, a Access, key Key, site string, auth devgate.Authorization, op *Operation) error {
	def, ok := registry[key]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownSetting, key)
	}
	if !a.AccessTo(def).Editable() {
		return fmt.Errorf("%w (%s)", ErrSettingNotWritable, key)
	}
	// No precondition check here, and that is not an omission: clearing
	// restores the default, and every default is a value the deployment
	// can always honour. privacy.ip_storage falls back to masked, which
	// needs no key.

	before, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return err
	}

	if err := s.ResetGuardedSetting(ctx, key, site, auth); err != nil {
		return err
	}

	auditID, err := s.recordForReturningID(ctx, a.Principal, AuditEntry{
		Action: ActionSettingReset,
		SiteID: site,
		Target: string(key),
		Detail: map[string]any{
			"from":    before,
			"to":      def.Default,
			"guarded": def.RequiresDeveloperPassword,
		},
	})
	if err != nil {
		return err
	}
	op.LinkAudit(ctx, auditID)
	return nil
}

func actorIDOf(p Principal) *int64 {
	if p.UserID == 0 {
		return nil
	}
	id := p.UserID
	return &id
}

// DeveloperPasswordPrompt is everything the panel needs to render the
// prompt for one change: why this setting is guarded, and the standing
// rule behind it.
type DeveloperPasswordPrompt struct {
	// Keys are the guarded settings this prompt covers.
	Keys []Key
	// Reasons pairs each key's label with why it is guarded.
	Reasons []DeveloperPasswordReason
	// Notice is the standing rule.
	Notice string
	// FormField is what to name the password input.
	FormField string
	// Configured is false when this deployment has no developer
	// password, in which case the change cannot be made from the panel
	// at all and the prompt should say so instead of asking.
	Configured bool
	// Unavailable is the sentence to show when Configured is false.
	Unavailable string
	// Entitled reports whether this principal may attempt the password.
	//
	// False for a customer, however complete their rights on their own
	// panel. When it is false the panel must render Locked and no field:
	// a password box in front of somebody who cannot have the password
	// is an invitation to go looking for it, and every attempt they make
	// costs the operator part of a shared failure budget.
	Entitled bool
	// Locked is what to show instead of the field.
	Locked string
}

// PromptFor builds the prompt for a set of settings about to be changed,
// as this principal should see it.
func PromptFor(a Access, configured bool, keys ...Key) DeveloperPasswordPrompt {
	prompt := DeveloperPasswordPrompt{
		Notice:      devgate.Notice,
		FormField:   devgate.FormField,
		Configured:  configured,
		Unavailable: devgate.NoticeNotConfigured,
		Entitled:    a.MayAttemptDeveloperPassword(),
		Locked:      LockNoticeLegal,
	}
	for _, key := range keys {
		def, ok := registry[key]
		if !ok || !def.RequiresDeveloperPassword {
			continue
		}
		prompt.Keys = append(prompt.Keys, key)
		prompt.Reasons = append(prompt.Reasons, DeveloperPasswordReason{
			Key:    key,
			Label:  def.Label,
			Reason: def.GateReason,
		})
	}
	return prompt
}

// DeveloperPasswordReason is one guarded setting and why it is guarded.
type DeveloperPasswordReason struct {
	Key    Key
	Label  string
	Reason string
}

// String renders the prompt as plain text, for a CLI or a log line.
func (p DeveloperPasswordPrompt) String() string {
	var b strings.Builder
	switch {
	case !p.Entitled:
		// Checked before Configured, because whether this deployment has
		// a developer password is not the customer's problem and telling
		// them it is missing would read as an invitation to fix it.
		b.WriteString(p.Locked)
	case !p.Configured:
		return p.Unavailable
	default:
		b.WriteString(p.Notice)
	}
	for _, reason := range p.Reasons {
		fmt.Fprintf(&b, "\n\n- %s: %s", reason.Label, reason.Reason)
	}
	return b.String()
}
