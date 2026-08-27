package web

import (
	"context"
	"errors"

	"github.com/cruciblelab/crucible-analytic/internal/mail"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// Sending a link to the person it belongs to.
//
// # The rule this file exists to keep
//
// The link is always on the screen. Whether mail is configured, whether
// it worked, whether it silently vanished into a spam folder - the
// person who minted the link is looking at it and can pass it on
// themselves.
//
// That is not caution, it is the whole reason C7.1 and C7.2 were built
// without email in the first place. A reset that says "sent" and never
// arrives is the worst failure available: the person waits, nobody is
// alarmed, and the thing that went wrong is invisible from both ends. So
// email here is an extra copy of something the operator already has,
// never the only copy.
//
// Which is why nothing in this file returns an error. A failed send is
// reported next to a link that still works.

// mailDelivery is what happened when the panel tried to email a link.
//
// Shown beside the link rather than instead of it. The three states are
// deliberately distinct: not configured, sent, and tried-and-failed are
// three different things for the operator to do next, and collapsing any
// two of them loses the instruction.
type mailDelivery struct {
	// Attempted is whether a send was tried at all. False means no mail
	// account is configured or it is switched off - not a failure, and
	// the note says so without alarm.
	Attempted bool
	// Sent is whether the server accepted the message. Accepted, not
	// delivered: nothing here can know the second, and the note says
	// that too.
	Sent bool
	// To is the address it went to, shown so the operator can see the
	// panel used the address they meant.
	To string
	// Note is the sentence for the page.
	Note string
	// Detail is the diagnosis when it failed, already turned into a
	// sentence. Empty otherwise.
	Detail string
}

// deliverLink emails a link, and reports what happened.
//
// The subject and body come from the catalog by key, with the link
// appended as its own line - never interpolated into a sentence. A link
// on its own line survives every mail client's idea of where a URL ends,
// and a URL wrapped in prose is the one that arrives cut in half.
func (s *Server) deliverLink(ctx context.Context, lang *ui.Language,
	to, subjectKey, bodyKey, link string) mailDelivery {

	out := mailDelivery{To: to}

	cfg, err := s.Store.MailConfig(ctx, s.SecretKey)
	if err != nil {
		switch {
		case errors.Is(err, panel.ErrNoMailAccount):
			// The ordinary state for a deployment that never set mail
			// up, and for one that switched it off on purpose. Not a
			// warning: the link below works.
			out.Note = lang.T("posta.teslim.kapali")
		case errors.Is(err, sealed.ErrCannotOpen):
			out.Attempted = true
			out.Note = lang.T("posta.teslim.basarisiz")
			out.Detail = lang.T("posta.hata.acilamiyor")
		default:
			s.logger().Error("panel: reading the mail account for a link", "err", err)
			out.Attempted = true
			out.Note = lang.T("posta.teslim.basarisiz")
			out.Detail = lang.T("posta.hata.okunamadi")
		}
		return out
	}

	cfg.Timeout = mailProbeTimeout
	out.Attempted = true

	probe := cfg.Send(mail.Message{
		To:      to,
		Subject: lang.T(subjectKey),
		Body:    lang.T(bodyKey) + "\n\n" + link + "\n",
	})

	// Recorded on the account, the same as a verification. The question
	// "why did nobody get the invitation" is asked days later, and the
	// answer has to have been written down at the time.
	if err := s.Store.RecordMailVerification(ctx, probe); err != nil {
		s.logger().Error("panel: recording a link delivery", "err", err)
	}

	if probe.Sent {
		out.Sent = true
		out.Note = lang.Tf("posta.teslim.gonderildi", to)
		return out
	}

	out.Note = lang.T("posta.teslim.basarisiz")
	out.Detail = lang.T("posta.tani." + string(probe.Diagnose()))
	s.logger().Warn("panel: a link could not be emailed",
		"to", to, "diagnosis", string(probe.Diagnose()), "stage", string(probe.Stage))
	return out
}
