package relupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

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
	return req.ToVersion, false, nil
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
