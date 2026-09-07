package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
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
	// Secrets marks the one that is the configuration rather than
	// tables, so the page can say that it is a separate file and that
	// it needs the password.
	Secrets bool
}

// backupRow is one catalogue entry as the page shows it.
type backupRow struct {
	// ID is what the Doğrula button sends back. It is the catalogue
	// row's id and nothing else: the panel is not granted the path
	// column and must not learn where the file is.
	ID      int64
	TakenAt time.Time
	Sets    []string
	Bytes   int64
	Version string
	// Missing means the file the row names is not on the disk any more.
	Missing bool

	// Checked is when this file was last opened and measured, nil when
	// it never has been.
	Checked *time.Time
	// Problems is what that check found, empty when it found nothing.
	//
	// A nil Checked and an empty Problems are not the same state and
	// the page must not draw them the same way: "nobody has ever
	// looked" is the answer this whole section exists to stop being the
	// answer.
	Problems string
}

// Verdict is what the page says about this row, as one of three words.
//
// A method rather than three booleans, so the template cannot render a
// combination that does not exist - "checked and unchecked" is not a
// state, and a template with two independent flags can draw it.
func (r backupRow) Verdict() string {
	switch {
	case r.Checked == nil:
		return "bakilmadi"
	case r.Problems != "":
		return "bozuk"
	default:
		return "saglam"
	}
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

	// KeepNote is the sentence about the age limit, always present.
	//
	// Always, and that is the requirement rather than a nicety. A backup
	// holds rows the retention policy has since deleted, so a backup
	// directory is the one place that number quietly does not apply -
	// and a customer reading a retention promise on one page and an
	// unbounded pile of backups on another has been told two things
	// that cannot both be true.
	//
	// Three sentences, because there are three states and only one of
	// them is about a number. A limit is in force; no limit is in force;
	// or nobody has said, which happens between installing this version
	// and the first upgrader pass and is a fact about the upgrader
	// rather than about backups.
	KeepNote string

	// AskingForPassword is whether the form shows the developer
	// password field, which is what the configuration set needs.
	//
	// False on a deployment with no developer password configured, and
	// the configuration set is then not offered at all: the gate is
	// shut, so a request for it could only be refused. Saying that on
	// the page is better than a checkbox whose only outcome is a
	// refusal.
	AskingForPassword bool

	// Notice is what to say after a press.
	Notice string
	Failed bool
}

// backupStatusFor gathers the section.
//
// Takes the store rather than reading it off the server, and a context
// rather than a request, so the section can be built with neither a
// database nor an HTTP request. See stores.go.
func (s *Server) backupStatusFor(ctx context.Context, db backupReader, lang *ui.Language,
	access panel.Access) (backupSection, string) {

	status, err := db.BackupStatus(ctx, access)
	if err != nil {
		s.logger().Error("panel: reading the backup status", "err", err)
		return backupSection{}, lang.T("saglik.yedek.okunamadi")
	}

	section := backupSection{
		Allowed: status.Allowed,
		Latest:  status.Latest,
		Running: status.Latest.InFlight(),
		// Only where there is a password to ask for. See the field.
		AskingForPassword: status.Allowed && s.Gate != nil && s.Gate.Configured(),
	}
	// Every set this build knows, with the panel one ticked.
	//
	// The small one by default rather than everything: it is the set
	// that cannot be rebuilt from anywhere, and a default that included
	// the traffic tables would make the first press on a large
	// deployment the one that gets refused for space.
	for _, set := range backup.Sets {
		if set.Secrets && !section.AskingForPassword {
			continue
		}
		section.Sets = append(section.Sets, backupSet{
			Name:    set.Name,
			Label:   lang.T("saglik.yedek.kume." + set.Name),
			Checked: set.Name == backup.SetPanel,
			Secrets: set.Secrets,
		})
	}
	if status.Latest != nil {
		section.LatestState = lang.T("saglik.yedek.durum." + string(status.Latest.State))
	}
	for _, b := range status.Backups {
		section.Backups = append(section.Backups, backupRow{
			ID:       b.ID,
			TakenAt:  b.TakenAt,
			Sets:     b.Sets,
			Bytes:    b.Bytes,
			Version:  b.Version,
			Missing:  b.State == "missing",
			Checked:  b.VerifiedAt,
			Problems: b.VerifyProblems,
		})
		// Only what is still there is counted. A total that included
		// files somebody deleted would be a number about the disk that
		// the disk disagrees with.
		if b.State != "missing" {
			section.TotalBytes += b.Bytes
		}
	}
	section.KeepNote = keepNote(lang, status.Policy)
	return section, ""
}

// keepNote turns the recorded policy into the sentence the page shows.
//
// Its own function so the three states are visible together. Written as
// a switch on the policy rather than as a template condition, because a
// template that decided this would put the reasoning in the one place
// nothing can test it.
func keepNote(lang *ui.Language, p backup.Policy) string {
	switch {
	case !p.Known():
		return lang.T("saglik.yedek.saklama.bilinmiyor")
	case p.KeepsForever():
		return lang.T("saglik.yedek.saklama.sinirsiz")
	default:
		return lang.Tf("saglik.yedek.saklama.gun", p.KeepDays)
	}
}

// backupPost queues one.
func (s *Server) backupPost(r *http.Request, db backupStore, lang *ui.Language,
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

	op, opErr := db.BeginOperation(r.Context(), access,
		panel.ActionBackupRequested, "backup", "")
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the backup", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	// Verified whatever was ticked, for the reason releasePost gives:
	// reading the password only when the configuration set is chosen
	// would leak, through timing, which sets a request named. An empty
	// field costs no argon2 work and is not counted as a failure, so
	// the ordinary data backup pays nothing for this.
	var auth devgate.Authorization
	if s.Gate != nil {
		result := s.Gate.Verify(r.Context(), devgate.RequestFrom(r,
			access.Principal.Label, panel.SecretsGateAction))
		if result.OK() {
			auth = result.For(panel.SecretsGateAction)
		}
	}

	req, err := db.RequestBackup(r.Context(), access, auth, op.ID(), chosen)

	section, sectionErr := s.backupStatusFor(r.Context(), db, lang, access)
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

// backupVerifyPost queues a check of one backup.
//
// Its own handler rather than a branch inside backupPost, because the
// two take different things from the form and mean different things: a
// take is a choice of sets, a check is a choice of file. Sharing a
// handler would mean a request that named both, and the first line of
// either would have to decide which one the person meant.
func (s *Server) backupVerifyPost(r *http.Request, db backupStore, lang *ui.Language,
	access panel.Access) (backupSection, string) {

	id, ok := s.backupIDFrom(r)
	if !ok {
		section, sectionErr := s.backupStatusFor(r.Context(), db, lang, access)
		if sectionErr != "" {
			return section, sectionErr
		}
		section.Notice = lang.T("saglik.yedek.dogrula_secim_yok")
		section.Failed = true
		return section, ""
	}

	op, opErr := db.BeginOperation(r.Context(), access,
		panel.ActionBackupVerified, "backup", strconv.FormatInt(id, 10))
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the check", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	req, err := db.VerifyBackup(r.Context(), access, op.ID(), id)

	section, sectionErr := s.backupStatusFor(r.Context(), db, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}

	if err != nil {
		section.Notice = backupErrorText(lang, err)
		section.Failed = true
		log.Warn("panel: backup check refused", "err", err, "backup", id)
		op.Step("istek yaz", false, "")
		notRolledBack := false
		_ = op.Finish(r.Context(), outcomeFor(err), err, &notRolledBack)
		return section, ""
	}

	section.Notice = lang.T("saglik.yedek.dogrulaniyor")
	section.Latest = req
	section.Running = true
	log.Info("panel: backup check requested", "request", req.ID, "backup", id)
	op.Step("istek yaz", true, "")
	rolledBack := false
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, &rolledBack)
	return section, ""
}

// backupRestorePost queues a restore of one backup into the side
// database.
//
// The same shape as backupVerifyPost and sharing its parsing, because
// the two forms carry the same field and differ only in what the
// upgrader then does. What is *not* shared is the action name: the
// dispatch in health.go decides between them, so a form that named the
// wrong one cannot be turned into the other by anything here.
func (s *Server) backupRestorePost(r *http.Request, db backupStore, lang *ui.Language,
	access panel.Access) (backupSection, string) {

	id, ok := s.backupIDFrom(r)
	if !ok {
		section, sectionErr := s.backupStatusFor(r.Context(), db, lang, access)
		if sectionErr != "" {
			return section, sectionErr
		}
		section.Notice = lang.T("saglik.yedek.dogrula_secim_yok")
		section.Failed = true
		return section, ""
	}

	op, opErr := db.BeginOperation(r.Context(), access,
		panel.ActionBackupRestored, "backup", strconv.FormatInt(id, 10))
	if opErr != nil {
		s.logger().Warn("panel: could not open an operation record for the restore", "err", opErr)
	}
	log := s.logger().With(logsink.OperationKey, op.ID())

	req, err := db.RestoreBackup(r.Context(), access, op.ID(), id)

	section, sectionErr := s.backupStatusFor(r.Context(), db, lang, access)
	if sectionErr != "" {
		_ = op.Finish(r.Context(), panel.OutcomeFailed, errors.New(sectionErr), nil)
		return section, sectionErr
	}

	if err != nil {
		section.Notice = backupErrorText(lang, err)
		section.Failed = true
		log.Warn("panel: backup restore refused", "err", err, "backup", id)
		op.Step("istek yaz", false, "")
		notRolledBack := false
		_ = op.Finish(r.Context(), outcomeFor(err), err, &notRolledBack)
		return section, ""
	}

	section.Notice = lang.T("saglik.yedek.yukleniyor")
	section.Latest = req
	section.Running = true
	log.Info("panel: backup restore requested", "request", req.ID, "backup", id)
	op.Step("istek yaz", true, "")
	rolledBack := false
	_ = op.Finish(r.Context(), panel.OutcomeSucceeded, nil, &rolledBack)
	return section, ""
}

// backupIDFrom reads which backup a row's button named.
//
// Parsed rather than trusted, and refused rather than defaulted: a zero
// id is not a backup, and asking for one would write a row the upgrader
// could only fail. Shared by the two buttons because a badly formed id
// means the same thing to both.
func (s *Server) backupIDFrom(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("yedek")), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
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
	case errors.Is(err, panel.ErrSecretsPasswordRequired):
		return lang.T("saglik.yedek.parola_gerekli")
	case errors.Is(err, backup.ErrNoRestoreTarget):
		return lang.T("saglik.yedek.yukleme_yok")
	case errors.Is(err, backup.ErrMixedRequest):
		// Its own sentence, because it is the one refusal here that is
		// about the product's design rather than about this press. A
		// person who ticked both boxes did something reasonable and has
		// to be told why it is two operations.
		return lang.T("saglik.yedek.ayri_dosya")
	default:
		// An unknown set, or nothing ticked at all. The queue's message
		// names which, and it is the only one here written for a person
		// who chose something impossible rather than for one who chose
		// nothing.
		return lang.T("saglik.yedek.secim_yok")
	}
}
