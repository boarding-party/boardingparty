// Package runs creates and finishes persona runs.
package runs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/store"
)

// ErrNotActive means the token is unknown or its run has already finished.
var ErrNotActive = errors.New("run not active")

type Launcher interface {
	Start(ctx context.Context, r store.Run, p project.Project) (session string, err error)
	Stop(ctx context.Context, session string) error
}

type Service struct {
	Store     *store.Store
	Artifacts artifacts.Store
	Launcher  Launcher
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
	if err := s.Store.CreateRun(r); err != nil {
		return store.Run{}, err
	}
	session, err := s.Launcher.Start(ctx, r, p)
	if err != nil {
		r.State, r.Outcome = store.RunFailed, err.Error()
		if uerr := s.Store.UpdateRun(r); uerr != nil {
			return r, errors.Join(err, uerr)
		}
		return r, err
	}
	r.State, r.Session = store.RunRunning, session
	if err := s.Store.UpdateRun(r); err != nil {
		return r, err
	}
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

// Finish records the final state and stops the run's session.
func (s *Service) Finish(ctx context.Context, token string, state store.RunState, outcome string) error {
	r, err := s.Active(token)
	if err != nil {
		return err
	}
	r.State, r.Outcome, r.Finished = state, outcome, time.Now().UTC()
	if err := s.Store.UpdateRun(r); err != nil {
		return err
	}
	if r.Session == "" {
		return nil
	}
	return s.Launcher.Stop(ctx, r.Session)
}
