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
func (s *Store) GateRequest(p Principal, r *http.Request, keys ...Key) devgate.Request {
	actions := make([]string, 0, len(keys))
	for _, key := range keys {
		if def, ok := registry[key]; ok && def.RequiresDeveloperPassword {
			actions = append(actions, GateAction(key))
		}
	}
	return devgate.Request{
		Actions:   actions,
		Password:  devgate.FromRequest(r),
		Actor:     p.Label,
		ActorKind: string(p.Kind),
		ActorID:   p.UserID,
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
func (s *Store) ApplySetting(ctx context.Context, p Principal, key Key, site string, value any, auth devgate.Authorization) error {
	before, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return err
	}

	if err := s.SetGuardedSetting(ctx, key, site, value, actorIDOf(p), auth); err != nil {
		return err
	}

	// Recorded after the write succeeded, so the log never claims a
	// change that did not happen. The reverse ordering - record, then
	// write - would produce an audit trail that is wrong in the
	// direction nobody checks.
	return s.RecordFor(ctx, p, AuditEntry{
		Action: ActionSettingChanged,
		SiteID: site,
		Target: string(key),
		Detail: map[string]any{
			"from":    before,
			"to":      value,
			"guarded": registry[key].RequiresDeveloperPassword,
		},
	})
}

// ClearSetting removes a stored value so the default applies again, and
// records it.
//
// Guarded exactly like a write, because it is one: for
// campaign.drop_params the default is the empty list, so clearing it
// means starting to store utm_term again.
func (s *Store) ClearSetting(ctx context.Context, p Principal, key Key, site string, auth devgate.Authorization) error {
	before, err := s.GetSetting(ctx, key, site)
	if err != nil {
		return err
	}

	if err := s.ResetGuardedSetting(ctx, key, site, auth); err != nil {
		return err
	}

	after := any(nil)
	if def, ok := registry[key]; ok {
		after = def.Default
	}
	return s.RecordFor(ctx, p, AuditEntry{
		Action: ActionSettingReset,
		SiteID: site,
		Target: string(key),
		Detail: map[string]any{
			"from":    before,
			"to":      after,
			"guarded": registry[key].RequiresDeveloperPassword,
		},
	})
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
}

// PromptFor builds the prompt for a set of settings about to be changed.
func PromptFor(configured bool, keys ...Key) DeveloperPasswordPrompt {
	prompt := DeveloperPasswordPrompt{
		Notice:      devgate.Notice,
		FormField:   devgate.FormField,
		Configured:  configured,
		Unavailable: devgate.NoticeNotConfigured,
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
	if !p.Configured {
		return p.Unavailable
	}
	var b strings.Builder
	b.WriteString(p.Notice)
	for _, reason := range p.Reasons {
		fmt.Fprintf(&b, "\n\n- %s: %s", reason.Label, reason.Reason)
	}
	return b.String()
}
