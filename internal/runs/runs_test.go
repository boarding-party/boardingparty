package runs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/store"
)

type fakeLauncher struct {
	mu               sync.Mutex
	started, stopped []string
	startErr         error
	phases           map[string]phaseInfo // by session; missing = session gone
}

type phaseInfo struct{ phase, msg string }

func (f *fakeLauncher) Phase(_ context.Context, session string) (string, bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.phases[session]
	return p.phase, ok, p.msg, nil
}

func (f *fakeLauncher) Start(_ context.Context, r store.Run, _ project.Project) (string, error) {
	if f.startErr != nil {
		return "", f.startErr
	}
	f.started = append(f.started, r.ID)
	return "sess-" + r.ID, nil
}

func (f *fakeLauncher) Stop(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, session)
	return nil
}

func newSvc(t *testing.T) (*Service, *fakeLauncher) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := project.Project{Name: "p", Sites: []string{"https://x.test"}, Roles: []project.Role{{
		Name:     "buyer",
		Personas: []project.Persona{{Name: "ann"}},
		Stories:  []project.Story{{Name: "checkout", Goal: "buy a thing"}},
	}}}
	if err := st.PutProject(p); err != nil {
		t.Fatal(err)
	}
	fl := &fakeLauncher{}
	return &Service{Store: st, Artifacts: artifacts.NewFS(t.TempDir(), 1024), Launcher: fl}, fl
}

func TestStartUnknownPersonaOrStory(t *testing.T) {
	svc, fl := newSvc(t)
	if _, err := svc.Start(context.Background(), "nobody", "checkout"); err == nil {
		t.Error("unknown persona: expected error")
	}
	if _, err := svc.Start(context.Background(), "ann", "nope"); err == nil {
		t.Error("unknown story: expected error")
	}
	if len(fl.started) != 0 {
		t.Errorf("launcher started: %v", fl.started)
	}
}

func TestStartCreatesRunningRun(t *testing.T) {
	svc, _ := newSvc(t)
	r, err := svc.Start(context.Background(), "ann", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Token) != 64 || r.State != store.RunRunning || r.Session != "sess-"+r.ID || r.Role != "buyer" || r.Started.IsZero() {
		t.Fatalf("bad run: %+v", r)
	}
	got, err := svc.Active(r.Token)
	if err != nil || got.ID != r.ID {
		t.Fatalf("active: %+v %v", got, err)
	}
}

func TestStartLauncherErrorFailsRun(t *testing.T) {
	svc, fl := newSvc(t)
	fl.startErr = errors.New("boom")
	if _, err := svc.Start(context.Background(), "ann", "checkout"); err == nil {
		t.Fatal("expected error")
	}
	rs, _ := svc.Store.Runs(10)
	if len(rs) != 1 || rs[0].State != store.RunFailed || !strings.Contains(rs[0].Outcome, "boom") {
		t.Fatalf("runs: %+v", rs)
	}
}

func TestFinishTwiceIsNotActive(t *testing.T) {
	svc, _ := newSvc(t)
	r, _ := svc.Start(context.Background(), "ann", "checkout")
	if err := svc.Finish(context.Background(), r.Token, store.RunDone, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Finish(context.Background(), r.Token, store.RunDone, "ok"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("want ErrNotActive, got %v", err)
	}
	if _, err := svc.Active(r.Token); !errors.Is(err, ErrNotActive) {
		t.Fatalf("active after finish: %v", err)
	}
	if err := svc.Finish(context.Background(), "unknown", store.RunDone, ""); !errors.Is(err, ErrNotActive) {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestFinishStopsSession(t *testing.T) {
	svc, fl := newSvc(t)
	r, _ := svc.Start(context.Background(), "ann", "checkout")
	if err := svc.Finish(context.Background(), r.Token, store.RunStuck, "stuck at pay"); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 1 || fl.stopped[0] != r.Session {
		t.Fatalf("stopped: %v", fl.stopped)
	}
	got, _ := svc.Store.RunByToken(r.Token)
	if got.State != store.RunStuck || got.Outcome != "stuck at pay" || got.Finished.IsZero() {
		t.Fatalf("run: %+v", got)
	}
}

func TestFinishRejectsNonTerminalState(t *testing.T) {
	svc, fl := newSvc(t)
	r, _ := svc.Start(context.Background(), "ann", "checkout")
	for _, st := range []store.RunState{store.RunPending, store.RunRunning, "bogus"} {
		err := svc.Finish(context.Background(), r.Token, st, "x")
		if err == nil || errors.Is(err, ErrNotActive) {
			t.Errorf("state %q: want ErrBadState-like error, got %v", st, err)
		}
		if !errors.Is(err, ErrBadState) {
			t.Errorf("state %q: want ErrBadState, got %v", st, err)
		}
	}
	if _, err := svc.Active(r.Token); err != nil || len(fl.stopped) != 0 {
		t.Fatalf("run must still be active: %v %v", err, fl.stopped)
	}
}

func TestFinishConcurrentOnlyOneWins(t *testing.T) {
	svc, fl := newSvc(t)
	r, _ := svc.Start(context.Background(), "ann", "checkout")
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- svc.Finish(context.Background(), r.Token, store.RunDone, "ok") }()
	}
	var nils, notActive int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			nils++
		case errors.Is(err, ErrNotActive):
			notActive++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if nils != 1 || notActive != 1 || len(fl.stopped) != 1 {
		t.Fatalf("nils=%d notActive=%d stopped=%v", nils, notActive, fl.stopped)
	}
}
