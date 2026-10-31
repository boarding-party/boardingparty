package runs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/boarding-party/boardingparty/internal/store"
)

// Watch reconciles active runs every `every` until ctx is cancelled.
func (s *Service) Watch(ctx context.Context, every, timeout time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Reconcile(ctx, time.Now().UTC(), timeout); err != nil {
			log.Printf("run watchdog: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile makes one pass over active runs and fails those whose session is
// gone, failed, ended without finish_run, or that outlived timeout. Runs are
// finished through the store's conditional update, so a concurrent finish_run
// from the agent always wins.
func (s *Service) Reconcile(ctx context.Context, now time.Time, timeout time.Duration) error {
	active, err := s.Store.ActiveRuns()
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range active {
		reason, err := s.verdict(ctx, r, now, timeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", r.ID, err))
			continue
		}
		if reason == "" {
			continue
		}
		won, err := s.Store.FinishRun(r.ID, store.RunFailed, reason, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", r.ID, err))
			continue
		}
		if won {
			session := r.Session
			if session == "" {
				session = s.Launcher.SessionName(r.ID) // may be an orphan from a crashed Start
			}
			if err := s.Launcher.Stop(ctx, session); err != nil {
				errs = append(errs, fmt.Errorf("run %s: stop session: %w", r.ID, err))
			}
		}
	}
	if err := s.reap(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reap stops run sessions whose run is finished or unknown to the store, so a
// failed Stop never leaks a session for good.
func (s *Service) reap(ctx context.Context) error {
	sessions, err := s.Launcher.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	var errs []error
	for session, runID := range sessions {
		r, err := s.Store.RunByID(runID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			errs = append(errs, fmt.Errorf("run %s: %w", runID, err))
			continue
		case r.State != store.RunDone && r.State != store.RunStuck && r.State != store.RunFailed:
			continue
		}
		if err := s.Launcher.Stop(ctx, session); err != nil {
			errs = append(errs, fmt.Errorf("reap session %s: %w", session, err))
		}
	}
	return errors.Join(errs...)
}

// verdict returns why r must be failed, or "" if it should be left alone.
func (s *Service) verdict(ctx context.Context, r store.Run, now time.Time, timeout time.Duration) (string, error) {
	if r.Session != "" {
		phase, exists, msg, err := s.Launcher.Phase(ctx, r.Session)
		if err != nil {
			return "", err
		}
		switch {
		case !exists:
			return "lost on restart", nil
		case phase == "Failed":
			return "session failed: " + msg, nil
		case phase == "Done":
			return "session ended without finish_run", nil
		}
	}
	if now.Sub(r.Started) > timeout {
		return "no finish within " + timeout.String(), nil
	}
	return "", nil
}
