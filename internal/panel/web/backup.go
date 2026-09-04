package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The surface for the backup the rest of F1b built.
//
// A queue with a runner and no page is the same thing as no feature: the
// customer experiences "there are no backups". And a queue with a runner
// and no *producer* is the defect this project has found twice - the
// button that writes a row nothing reads, in reverse.
//
// So the button ships in the same commit as the runner.

// backupSet is one choosable set, with the words for it.
type backupSet struct {
	Name    string
	Label   string
	Checked bool
}

// backupRow is one catalogue entry as the page shows it.
type backupRow struct {
	TakenAt time.Time
	Sets    []string
	Bytes   int64
	Version string
	// Missing means the file the row names is not on the disk any more.
	Missing bool
}

// backupSection is the panel on the health page.
type backupSection struct {
	// Allowed is whether this principal may ask for one.
	Allowed bool
	// Sets are the choices, in the order internal/backup declares them.
	Sets []backupSet

	// Latest is the most recent request, nil when there has never been
	// one.
	Latest *backup.Request
	// LatestState is that request's state in the reader's language.
	LatestState string
	// Running is whether one is in flight, which is what makes the page
	// refresh itself.
	Running bool

	// Backups is the catalogue, newest first.
	Backups []backupRow
	// TotalBytes is what they occupy together.
	TotalBytes int64

	// Notice is what to say after a press.
	Notice string
	Failed bool
}

// backupStatusFor gathers the section.
func (s *Server) backupStatusFor(r *http.Request, lang *ui.Language,
	access panel.Access) (backupSection, string) {

	status, err := s.Store.BackupStatus(r.Context(), access)
	if err != nil {
		s.logger().Error("panel: reading the backup status", "err", err)
		return backupSection{}, lang.T("saglik.yedek.okunamadi")
	}

	section := backupSection{
		Allowed: status.Allowed,
		Latest:  status.Latest,
		Running: status.Latest.InFlight(),
	}
	// Every set this build knows, with the panel one ticked.
	//
	// The small one by default rather than everything: it is the set
	// that cannot be rebuilt from anywhere, and a default that included
	// the traffic tables would make the first press on a large
	// deployment the one that gets refused for space.
	for _, set := range backup.Sets {
		section.Sets = append(section.Sets, backupSet{
			Name:    set.Name,
			Label:   lang.T("saglik.yedek.kume." + set.Name),
			Checked: set.Name == backup.SetPanel,
		})
	}
	if status.Latest != nil {
		section.LatestState = lang.T("saglik.yedek.durum." + string(status.Latest.State))
	}
	for _, b := range status.Backups {
		section.Backups = append(section.Backups, backupRow{
			TakenAt: b.TakenAt,
			Sets:    b.Sets,
			Bytes:   b.Bytes,
			Version: b.Version,
			Missing: b.State == "missing",
		})
		// Only what is still there is counted. A total that included
		// files somebody deleted would be a number about the disk that
		// the disk disagrees with.
		if b.State != "missing" {
			section.TotalBytes += b.Bytes
		}
	}
	return section, ""
}

// backupPost queues one.
func (s *Server) backupPost(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access) (backupSection, string) {

	// Only the sets that were ticked, filtered against what this build
	// knows rather than trusted.
	//
	// A form is a list of strings from a browser. Passing them to the
	// queue unchecked would let a request name anything at all, and the
	// refusal would arrive one process later on a row somebody has to go
	// and read.
	var chosen []string
	for _, set := range backup.Sets {
		if r.FormValue("kume-"+set.Name) != "" {
			chosen = append(chosen, set.Name)
		}
	}

	op, opErr := s.Store.BeginOperation(r.Context(), access,
		panel.ActionBackupRequested, "backup", "")
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the backup", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	req, err := s.Store.RequestBackup(r.Context(), access, op.ID(), chosen)

	section, sectionErr := s.backupStatusFor(r, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}
	// The boxes the person ticked, echoed back, so a refused request does
	// not make them choose again.
	for i := range section.Sets {
		section.Sets[i].Checked = false
		for _, name := range chosen {
			if section.Sets[i].Name == name {
				section.Sets[i].Checked = true
			}
		}
	}

	if err != nil {
		section.Notice = backupErrorText(lang, err)
		section.Failed = true
		log.Warn("panel: backup request refused", "err", err, "sets", chosen)
		op.Step("istek yaz", false, "")
		notRolledBack := false
		_ = op.Finish(r.Context(), outcomeFor(err), err, &notRolledBack)
		return section, ""
	}

	section.Notice = lang.T("saglik.yedek.istendi")
	section.Latest = req
	section.Running = true
	log.Info("panel: backup requested", "request", req.ID, "sets", chosen)
	op.Step("istek yaz", true, "")
	ok := false
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, &ok)
	return section, ""
}

// backupErrorText turns a refusal into the sentence somebody reads.
//
// Each one says what to do next, because a refusal a person cannot act
// on is a refusal that becomes a support message.
func backupErrorText(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, panel.ErrBackupInFlight):
		return lang.T("saglik.yedek.zaten_var")
	case errors.Is(err, panel.ErrSettingNotWritable):
		return lang.T("saglik.yedek.yetki_yok")
	default:
		// An unknown set, or nothing ticked at all. The queue's message
		// names which, and it is the only one here written for a person
		// who chose something impossible rather than for one who chose
		// nothing.
		return lang.T("saglik.yedek.secim_yok")
	}
}
