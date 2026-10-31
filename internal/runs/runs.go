// Package runs creates and finishes persona runs.
package runs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/store"
)

// ErrNotActive means the token is unknown or its run has already finished.
// Defaults for the run watchdog.
const (
	DefaultRunTimeout   = 20 * time.Minute
	DefaultPollInterval = 15 * time.Second
)

var ErrNotActive = errors.New("run not active")

// ErrBusy means another run is pending or running. Plan 1 runs one at a time
// because a session can read every other session's spec (and so its token).
var ErrBusy = errors.New("another run is active; Plan 1 runs one at a time")

// ErrBadState means Finish was given a state that is not final.
var ErrBadState = errors.New("finish state must be done, stuck or failed")

type Launcher interface {
	Start(ctx context.Context, r store.Run, p project.Project) (session string, err error)
	Stop(ctx context.Context, session string) error
	// SessionName is the deterministic session name Start gives a run, so an
	// orphan can be stopped even if its name was never recorded.
	SessionName(runID string) string
	// Phase reports the session's phase (Pending, Running, Done, Failed), whether
	// it still exists, and its status message.
	Phase(ctx context.Context, session string) (phase string, exists bool, message string, err error)
	// Sessions lists the run sessions that exist, as session name -> run ID.
	Sessions(ctx context.Context) (map[string]string, error)
}

type Service struct {
	Store     *store.Store
	Artifacts artifacts.Store
	Launcher  Launcher

	startMu sync.Mutex // makes the one-active-run check and CreateRun atomic
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Start creates a run for persona doing story and launches its session.
func (s *Service) Start(ctx context.Context, persona, story string) (store.Run, error) {
	p, err := s.Store.Project()
	if err != nil {
		return store.Run{}, fmt.Errorf("load project: %w", err)
	}
	role, st, err := find(p, persona, story)
	if err != nil {
		return store.Run{}, err
	}
	id, err := randHex(16)
	if err != nil {
		return store.Run{}, err
	}
	token, err := randHex(32)
	if err != nil {
		return store.Run{}, err
	}
	r := store.Run{ID: id, Token: token, Persona: persona, Role: role, Story: st,
		State: store.RunPending, Started: time.Now().UTC()}
	s.startMu.Lock()
	active, err := s.Store.ActiveRuns()
	if err == nil && len(active) > 0 {
		err = ErrBusy
	}
	if err == nil {
		// A finished run's session may still be alive (failed Stop, deletion in
		// progress) and could read the next run's token.
		var sessions map[string]string
		if sessions, err = s.Launcher.Sessions(ctx); err != nil {
			err = fmt.Errorf("list sessions: %w", err)
		} else if len(sessions) > 0 {
			err = ErrBusy
		}
	}
	if err == nil {
		err = s.Store.CreateRun(r)
	}
	s.startMu.Unlock()
	if err != nil {
		return store.Run{}, err
	}
	session, err := s.Launcher.Start(ctx, r, p)
	if err != nil {
		r.State, r.Outcome, r.Finished = store.RunFailed, err.Error(), time.Now().UTC()
		if _, ferr := s.Store.FinishRun(r.ID, store.RunFailed, r.Outcome, r.Finished); ferr != nil {
			return r, errors.Join(err, ferr)
		}
		return r, err
	}
	won, err := s.Store.SetRunSession(r.ID, session)
	if err != nil {
		return r, err
	}
	if !won {
		// Finished (or failed by the watchdog) while launching: don't leak the session.
		return r, errors.Join(ErrNotActive, s.Launcher.Stop(ctx, session))
	}
	r.State, r.Session = store.RunRunning, session
	return r, nil
}

// find returns the role owning persona, and the goal of the named story in that role.
func find(p project.Project, persona, story string) (role, storyName string, err error) {
	for _, r := range p.Roles {
		for _, pe := range r.Personas {
			if pe.Name != persona {
				continue
			}
			for _, st := range r.Stories {
				if st.Name == story {
					return r.Name, st.Name, nil
				}
			}
			return "", "", fmt.Errorf("persona %q (role %q) has no story %q", persona, r.Name, story)
		}
	}
	return "", "", fmt.Errorf("unknown persona %q", persona)
}

// Active returns the run for token if it is pending or running.
func (s *Service) Active(token string) (store.Run, error) {
	r, err := s.Store.RunByToken(token)
	if errors.Is(err, store.ErrNotFound) {
		return store.Run{}, ErrNotActive
	}
	if err != nil {
		return store.Run{}, err
	}
	if r.State != store.RunPending && r.State != store.RunRunning {
		return store.Run{}, ErrNotActive
	}
	return r, nil
}

// Finish records the final state and stops the run's session. Only the
// caller that actually moves the run out of an active state stops the session.
func (s *Service) Finish(ctx context.Context, token string, state store.RunState, outcome string) error {
	switch state {
	case store.RunDone, store.RunStuck, store.RunFailed:
	default:
		return fmt.Errorf("%w: got %q", ErrBadState, state)
	}
	r, err := s.Active(token)
	if err != nil {
		return err
	}
	won, err := s.Store.FinishRun(r.ID, state, outcome, time.Now().UTC())
	if err != nil {
		return err
	}
	if !won {
		return ErrNotActive
	}
	if r.Session == "" {
		return nil
	}
	return s.Launcher.Stop(ctx, r.Session)
}
