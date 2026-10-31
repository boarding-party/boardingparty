package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/boarding-party/boardingparty/internal/project"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunRoundTrip(t *testing.T) {
	s := open(t)
	start := time.Now().UTC().Truncate(time.Second)
	r := Run{ID: "r1", Token: "tok", Persona: "Ada", Role: "visitor", Story: "todos",
		State: "pending", Session: "sess-1", Started: start}
	if err := s.CreateRun(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunByToken("tok")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "r1" || got.State != "pending" || got.Session != "sess-1" || !got.Started.Equal(start) {
		t.Fatalf("got %+v", got)
	}
	r.State, r.Outcome, r.Finished = "done", "ok", start.Add(time.Minute)
	if err := s.UpdateRun(r); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RunByToken("tok")
	if got.State != "done" || got.Outcome != "ok" || !got.Finished.Equal(start.Add(time.Minute)) {
		t.Fatalf("got %+v", got)
	}
	if err := s.AddBug(Bug{ID: "b1", RunID: "r1", Summary: "x", Severity: "high", Created: start}); err != nil {
		t.Fatal(err)
	}
	bugs, err := s.Bugs("r1")
	if err != nil || len(bugs) != 1 || bugs[0].Summary != "x" {
		t.Fatalf("bugs %+v %v", bugs, err)
	}
}

func TestRunByTokenUnknown(t *testing.T) {
	s := open(t)
	if _, err := s.RunByToken("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestActiveRunsExcludesFinished(t *testing.T) {
	s := open(t)
	now := time.Now().UTC()
	for i, st := range []RunState{"pending", "running", "done", "stuck", "failed"} {
		id := string(rune('a' + i))
		if err := s.CreateRun(Run{ID: id, Token: "t" + id, State: st, Started: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	act, err := s.ActiveRuns()
	if err != nil || len(act) != 2 {
		t.Fatalf("active %+v %v", act, err)
	}
	all, _ := s.Runs(3)
	if len(all) != 3 || all[0].ID != "e" {
		t.Fatalf("runs newest first: %+v", all)
	}
}

func TestPersonaStateRoundTrip(t *testing.T) {
	s := open(t)
	st, err := s.PersonaState("Ada")
	if err != nil || st != nil {
		t.Fatalf("want nil, got %s %v", st, err)
	}
	if err := s.SavePersonaState("Ada", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePersonaState("Ada", json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	st, _ = s.PersonaState("Ada")
	if string(st) != `{"a":2}` {
		t.Fatalf("got %s", st)
	}
}

func TestProjectReplace(t *testing.T) {
	s := open(t)
	if err := s.PutProject(project.Project{Name: "one", Sites: []string{"https://a.test"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProject(project.Project{Name: "two", Sites: []string{"https://b.test"}}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Project()
	if err != nil || p.Name != "two" {
		t.Fatalf("got %+v %v", p, err)
	}
}

func TestRunByID(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RunByID("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.CreateRun(Run{ID: "r1", Token: "t1", State: "pending"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RunByID("r1"); err != nil || got.Token != "t1" {
		t.Fatalf("got %+v %v", got, err)
	}
}
