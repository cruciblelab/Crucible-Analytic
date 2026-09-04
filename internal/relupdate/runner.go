package relupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The half that was missing: something that actually takes a queued
// request and does it.
//
// # What was wrong before this file
//
// V2 wrote the queue, V3 downloaded and verified packages, V4 installed
// them and put the old ones back, V5 put a button on the page. Every
// piece worked and had tests. Nothing called Claim.
//
// So pressing the button wrote a row that no process ever read. The
// page said "Sırada" and went on saying it, indefinitely, and the
// customer's only evidence that anything was wrong was that nothing
// happened. That is the worst shape a missing feature can take: it does
// not fail, it waits.
//
// It survived because every test in this package tests one link. Claim
// has a test, Fetch has a test, Install has several - and a chain of
// tested links is not a tested chain. The thing nobody had written was
// the sentence "and then the upgrader does it".
//
// *Her halkası test edilmiş bir zincir, test edilmiş bir zincir değildir.*

// StaleClaimAge is how long a claimed request may sit before another
// pass takes it back.
//
// Twenty minutes, matching StaleAfter: a download of forty megabytes on
// a slow link plus an install is minutes, and an upgrader killed
// mid-install leaves binaries already put back by Install's own
// rollback. Reclaiming sooner would start a second install while the
// first was still copying.
const StaleClaimAge = StaleAfter

// Runner takes one queued request and carries it out.
type Runner struct {
	Pool   *pgxpool.Pool
	Source Source
	// Install does the replacing. Its Restart is what decides whether
	// the new binaries are actually running afterwards; see Result.
	Install Installer
	// Name identifies this upgrader in the claim, so two of them are
	// distinguishable in the row.
	Name   string
	Logger *slog.Logger
	// WorkDir is where packages are unpacked. Empty means a temporary
	// directory, removed afterwards.
	WorkDir string

	// Restart asks the restarter to restart the services and reports
	// whether they came back. Zero value means no restarter: the
	// binaries are replaced and the page says a restart is needed,
	// which is what every deployment did before V4c.
	Restart Doorbell
}

// RunOnce claims one request and finishes it.
//
// Returns ErrNothingToDo when the queue is empty, which is not a
// failure and is what a caller should not log.
func (r Runner) RunOnce(ctx context.Context) (*Request, error) {
	// Stale claims first, for the reason the schema applier does the
	// same: an upgrader that died mid-request leaves a row nothing will
	// ever move, and the one in-flight slot means that row blocks every
	// future request rather than only its own.
	if freed, err := ExpireStale(ctx, r.Pool, StaleClaimAge); err != nil {
		r.logger().Warn("relupdate: could not expire stale claims", "err", err)
	} else if freed > 0 {
		r.logger().Warn("relupdate: released a claim nobody finished", "requests", freed)
	}

	req, err := Claim(ctx, r.Pool, r.name())
	switch {
	case errors.Is(err, ErrNothingToDo):
		return nil, ErrNothingToDo
	case err != nil:
		return nil, err
	case req == nil:
		return nil, ErrNothingToDo
	}

	log := r.logger().With("request", req.ID, "version", req.ToVersion)
	log.Info("relupdate: starting")

	installed, rolledBack, runErr := r.carryOut(ctx, req, log)

	state := StateSucceeded
	if runErr != nil {
		state = StateFailed
	}
	if finErr := Finish(ctx, r.Pool, req.ID, state, runErr, installed, rolledBack); finErr != nil {
		// The work already happened. Reporting the write failure rather
		// than the work's own outcome would lose the more important of
		// the two, so it is logged and the outcome is returned.
		log.Error("relupdate: could not record the outcome", "err", finErr)
	}
	if runErr != nil {
		log.Error("relupdate: failed", "err", runErr, "rolled_back", rolledBack)
		return req, runErr
	}
	log.Info("relupdate: done", "installed", installed)
	return req, nil
}

// carryOut is fetch, then install. Separated so RunOnce always records
// an outcome, whatever happens in here.
func (r Runner) carryOut(ctx context.Context, req *Request, log *slog.Logger) (string, bool, error) {
	if r.Source.BaseURL == "" || !r.Source.PublicKey.IsSet() {
		// Checked after the claim rather than before, deliberately. A
		// request queued on a deployment with no release source has to
		// end up *failed with a reason on the row*, not left pending
		// forever: the page is where somebody is waiting, and "nothing
		// is configured" is exactly the answer they need.
		return "", false, fmt.Errorf("%w: the upgrader has no release source configured; "+
			"set base_url and public_key in upgrader.toml", ErrNotConfigured)
	}

	dir := r.WorkDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "crucible-release-*")
		if err != nil {
			return "", false, err
		}
		// Removed whatever happens. A failed install that left forty
		// megabytes behind every time somebody retried would fill the
		// disk of the machine least able to spare it.
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}

	log.Info("relupdate: fetching")
	root, err := r.Source.Fetch(ctx, req.ToVersion, dir)
	if err != nil {
		// Nothing has been touched: Fetch downloads and verifies before
		// anything is replaced, so a failure here leaves the running
		// system exactly as it was. Said in the message, because "failed"
		// on a page about replacing binaries is a sentence that frightens
		// somebody who cannot see which half it failed in.
		return "", false, fmt.Errorf("the package could not be fetched or did not verify, "+
			"and nothing on this machine was changed: %w", err)
	}

	log.Info("relupdate: installing")
	res, err := r.Install.Install(ctx, root)
	if err != nil {
		return "", res.RolledBack, err
	}

	// ---- and then the part that decides whether it worked ----
	//
	// Everything above proves the files are ours and that each binary
	// runs when executed. Neither is the question. The question is
	// whether the *services* come back, and that is only answerable
	// after they have been restarted - which is why the checkpoint
	// outlives Install.
	if !r.Restart.Configured() {
		// No restarter installed. The install stands and the page says
		// a restart is needed; the checkpoint is kept, because nothing
		// has yet shown the new version working.
		log.Info("relupdate: installed; no restarter is configured, so the services are " +
			"still running the previous binaries until somebody restarts them")
		return req.ToVersion, false, nil
	}

	// The moment is read before the doorbell rings, and off the database
	// - see Doorbell.Since. Before, because a service that came back
	// between the ring and the reading would be dated earlier than the
	// restart it was answering. Off the database, because that is what
	// writes the rows this is about to be compared against.
	since, sinceErr := r.Restart.Since(ctx)
	if err := r.Restart.Ring(); err != nil {
		return req.ToVersion, false, fmt.Errorf("the binaries are installed and each one "+
			"runs, but the restart could not be requested: %w", err)
	}
	log.Info("relupdate: restart requested, waiting for the services to report back")
	if sinceErr != nil {
		// The restart still happened, which is the half that matters to
		// a running machine. What is lost is the ability to tell whether
		// it worked, and that is said rather than guessed at: a fallback
		// to this machine's clock here would be the defect this call
		// exists to remove, reintroduced on the path nobody watches.
		return req.ToVersion, false, fmt.Errorf("the binaries are installed and restarted, "+
			"and whether the services came back could not be determined: %w", sinceErr)
	}

	missing, err := r.Restart.Healthy(ctx, since)
	if err != nil {
		// The check itself failed - the database went away, or the
		// context ended. Not evidence the release is bad, so nothing is
		// rolled back: rolling back on "we could not tell" would undo
		// good releases every time the database blinked.
		return req.ToVersion, false, fmt.Errorf("the binaries are installed and restarted, "+
			"and whether the services came back could not be determined: %w", err)
	}
	if len(missing) == 0 {
		// Only now is the checkpoint worthless.
		if err := r.Install.Forget(res.Previous); err != nil {
			log.Warn("relupdate: could not remove the checkpoint", "err", err, "path", res.Previous)
		}
		return req.ToVersion, false, nil
	}

	// ---- the escape ----
	log.Error("relupdate: services did not come back; putting the previous binaries back",
		"missing", missing)
	rolledBack := true
	if err := r.Install.Restore(res.Previous); err != nil {
		// The worst outcome in this file, and it is reported as itself:
		// the new binaries are in place, the services are not healthy,
		// and the old ones could not be put back. Somebody has to be
		// told exactly that rather than "the update failed".
		return "", false, fmt.Errorf("%s did not come back after the update, and the "+
			"previous binaries could NOT be put back (%v). The machine needs somebody: "+
			"the checkpoint is at %s", strings.Join(missing, ", "), err, res.Previous)
	}
	back, backErr := r.Restart.Since(ctx)
	if err := r.Restart.Ring(); err != nil {
		return "", rolledBack, fmt.Errorf("%s did not come back, the previous binaries "+
			"were put back, and the restart that would start them could not be "+
			"requested: %w", strings.Join(missing, ", "), err)
	}
	if backErr != nil {
		return "", rolledBack, fmt.Errorf("%s did not come back after the update. The "+
			"previous binaries were put back and restarted, and whether they came back "+
			"could not be determined (%v). The machine needs somebody",
			strings.Join(missing, ", "), backErr)
	}
	stillMissing, healthErr := r.Restart.Healthy(ctx, back)
	if healthErr != nil || len(stillMissing) > 0 {
		return "", rolledBack, fmt.Errorf("%s did not come back after the update. The "+
			"previous binaries were put back and restarted, and %s are still not "+
			"reporting. The machine needs somebody",
			strings.Join(missing, ", "), strings.Join(stillMissing, ", "))
	}
	return "", rolledBack, fmt.Errorf("%s did not come back after the update, so the "+
		"previous version was put back and is running again. Nothing was lost; the "+
		"release was refused by this machine rather than by its signature",
		strings.Join(missing, ", "))
}

func (r Runner) name() string {
	if r.Name != "" {
		return r.Name
	}
	return "upgrader"
}

func (r Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
