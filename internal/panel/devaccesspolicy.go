package panel

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DevAccessPolicy is what this deployment does when the developer asks
// to come in.
//
// Resolved rather than read: the stored value is one of three words, but
// "open" only means open while KeyDevAccessOpenUntil is still in the
// future, and an expired window has to mean something. It means ask.
type DevAccessPolicy struct {
	// Mode is DevAccessAsk, DevAccessDeny or DevAccessOpen, after the
	// window has been taken into account. A stored "open" whose window
	// has closed resolves to DevAccessAsk here, so no caller has to
	// remember the rule.
	Mode string
	// OpenUntil is when an open window closes. Zero unless Mode is
	// DevAccessOpen.
	OpenUntil time.Time
	// Stored is the value as written, before the window was applied.
	// Only the settings page and the audit log care; it is what lets the
	// panel say "open, but the window closed" rather than silently
	// showing "ask" to somebody who believes they set it to open.
	Stored string
}

// Expired reports an open policy whose window has closed.
func (p DevAccessPolicy) Expired() bool {
	return p.Stored == DevAccessOpen && p.Mode != DevAccessOpen
}

// DevAccessPolicyFor resolves the deployment's policy.
//
// Failures resolve to asking. A database that cannot answer "what is the
// policy" must not be read as permission to walk in, and must not be
// read as a refusal either - refusing would strand the owner's own
// developer at the moment the deployment is least healthy. Asking is the
// answer that leaves a person in the loop, which is what the setting is
// for.
func (s *Store) DevAccessPolicyFor(ctx context.Context) DevAccessPolicy {
	p := DevAccessPolicy{Mode: DevAccessAsk, Stored: DevAccessAsk}

	stored, err := s.GetSetting(ctx, KeyDevAccessPolicy, "")
	if err != nil {
		slog.Default().Warn("panel: could not read the developer access policy; asking the owner",
			"err", err)
		return p
	}
	mode, _ := stored.(string)
	switch mode {
	case DevAccessDeny, DevAccessOpen:
		p.Stored = mode
	default:
		return p
	}
	if p.Stored == DevAccessDeny {
		p.Mode = DevAccessDeny
		return p
	}

	// Open, so the window decides.
	until, err := s.GetSetting(ctx, KeyDevAccessOpenUntil, "")
	if err != nil {
		slog.Default().Warn("panel: could not read the developer access window; asking the owner",
			"err", err)
		return p
	}
	text, _ := until.(string)
	if text == "" {
		return p
	}
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		// Refused at write time by checkOpenUntil, so reaching this means
		// the row predates the check or was written around it. Ask.
		slog.Default().Warn("panel: the developer access window is not a timestamp; asking the owner",
			"value", text)
		return p
	}
	if !time.Now().Before(at) {
		return p
	}
	p.Mode = DevAccessOpen
	p.OpenUntil = at
	return p
}

// SetDevAccessPolicy stores the policy and records who decided it.
//
// A method of its own rather than a plain SetSetting, because C8's
// requirement is that the decision reaches the audit log - and the
// generic settings path writes no audit entry for anything. Putting the
// record here rather than in the handler means the one call that changes
// this cannot be made without it.
//
// The window is written in the same call for the same reason: "open" and
// "open until when" are one decision, and a caller that could set the
// first without the second could leave the door open by forgetting an
// argument.
func (s *Store) SetDevAccessPolicy(ctx context.Context, p Principal, mode, openUntil string) error {
	switch mode {
	case DevAccessAsk, DevAccessDeny, DevAccessOpen:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidSetting, mode)
	}
	if mode != DevAccessOpen {
		// Cleared rather than left behind: a stale timestamp under a
		// policy of "ask" reads, on the settings page, as though the
		// deployment is one word away from being open until then.
		openUntil = ""
	}
	if err := s.setSetting(ctx, KeyDevAccessOpenUntil, "", openUntil, principalUserID(p)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, KeyDevAccessPolicy, "", mode, principalUserID(p)); err != nil {
		return err
	}

	entry := AuditEntry{
		Action: ActionDevAccessPolicySet,
		Target: string(KeyDevAccessPolicy),
		Detail: map[string]any{
			"policy": mode,
		},
	}
	if openUntil != "" {
		entry.Detail["open_until"] = openUntil
	}
	if err := s.RecordFor(ctx, p, entry); err != nil {
		s.logAuditFailure("developer access policy", err)
	}
	return nil
}

// principalUserID is the actor id a settings row records, or nil.
//
// panel_settings.updated_by is a foreign key to panel_users, so a
// developer or system principal - which has no row there - has to be
// stored as NULL rather than as zero. The audit entry still names them;
// that table records the label by value for exactly this reason.
func principalUserID(p Principal) *int64 {
	if p.UserID == 0 {
		return nil
	}
	id := p.UserID
	return &id
}
