package runs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tiny-systems/boardingparty/internal/store"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// seed creates a run in the given state started `age` before t0.
func seed(t *testing.T, svc *Service, id string, state store.RunState, session string, age time.Duration) {
	t.Helper()
	r := store.Run{ID: id, Token: "tok-" + id, Persona: "ann", Role: "buyer", Story: "checkout",
		State: state, Session: session, Started: t0.Add(-age)}
	if err := svc.Store.CreateRun(r); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, svc *Service, id string) store.Run {
	t.Helper()
	r, err := svc.Store.RunByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReconcileTimesOutSilentRun(t *testing.T) {
	svc, fl := newSvc(t)
	fl.phases = map[string]phaseInfo{"s1": {"Running", ""}}
	seed(t, svc, "r1", store.RunRunning, "s1", 21*time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	r := get(t, svc, "r1")
	if r.State != store.RunFailed || r.Outcome != "no finish within 20m0s" {
		t.Errorf("got %s %q", r.State, r.Outcome)
	}
	if len(fl.stopped) != 1 || fl.stopped[0] != "s1" {
		t.Errorf("stopped = %v", fl.stopped)
	}
}

func TestReconcileSessionGoneFailsRun(t *testing.T) {
	svc, fl := newSvc(t)
	seed(t, svc, "r1", store.RunRunning, "s1", time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	r := get(t, svc, "r1")
	if r.State != store.RunFailed || r.Outcome != "lost on restart" {
		t.Errorf("got %s %q", r.State, r.Outcome)
	}
	if len(fl.stopped) != 1 {
		t.Errorf("stopped = %v", fl.stopped)
	}
}

func TestReconcileLeavesHealthyRun(t *testing.T) {
	svc, fl := newSvc(t)
	fl.phases = map[string]phaseInfo{"s1": {"Running", ""}}
	seed(t, svc, "r1", store.RunRunning, "s1", 5*time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if r := get(t, svc, "r1"); r.State != store.RunRunning {
		t.Errorf("state = %s", r.State)
	}
	if len(fl.stopped) != 0 {
		t.Errorf("stopped = %v", fl.stopped)
	}
}

func TestReconcileSessionFailedPhase(t *testing.T) {
	svc, fl := newSvc(t)
	fl.phases = map[string]phaseInfo{"s1": {"Failed", "pod OOMKilled"}}
	seed(t, svc, "r1", store.RunRunning, "s1", time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	r := get(t, svc, "r1")
	if r.State != store.RunFailed || !strings.Contains(r.Outcome, "pod OOMKilled") {
		t.Errorf("got %s %q", r.State, r.Outcome)
	}
	if len(fl.stopped) != 1 {
		t.Errorf("stopped = %v", fl.stopped)
	}
}

func TestReconcileDonePhaseWithoutFinishFails(t *testing.T) {
	svc, fl := newSvc(t)
	fl.phases = map[string]phaseInfo{"s1": {"Done", ""}}
	seed(t, svc, "r1", store.RunRunning, "s1", time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	r := get(t, svc, "r1")
	if r.State != store.RunFailed || !strings.Contains(r.Outcome, "without finish_run") {
		t.Errorf("got %s %q", r.State, r.Outcome)
	}
}

func TestReconcilePendingWithoutSessionLeftAloneUntilTimeout(t *testing.T) {
	svc, fl := newSvc(t)
	seed(t, svc, "young", store.RunPending, "", time.Minute)
	seed(t, svc, "old", store.RunPending, "", 21*time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if r := get(t, svc, "young"); r.State != store.RunPending {
		t.Errorf("young state = %s", r.State)
	}
	if r := get(t, svc, "old"); r.State != store.RunFailed {
		t.Errorf("old state = %s", r.State)
	}
	if len(fl.stopped) != 1 || fl.stopped[0] != "sess-old" {
		t.Errorf("only the timed-out run's possible orphan is stopped: %v", fl.stopped)
	}
}

func TestReconcileDoesNotOverwriteFinishedRun(t *testing.T) {
	svc, _ := newSvc(t)
	seed(t, svc, "r1", store.RunRunning, "s1", 30*time.Minute)
	won, err := svc.Store.FinishRun("r1", store.RunDone, "all good", t0)
	if err != nil || !won {
		t.Fatal(won, err)
	}
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if r := get(t, svc, "r1"); r.State != store.RunDone || r.Outcome != "all good" {
		t.Errorf("got %s %q", r.State, r.Outcome)
	}
}

func TestReconcileOrphanStopsDeterministicSession(t *testing.T) {
	svc, fl := newSvc(t)
	seed(t, svc, "r1", store.RunPending, "", 21*time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 1 || fl.stopped[0] != "sess-r1" {
		t.Errorf("stopped = %v", fl.stopped)
	}
}

func TestReconcileUnknownPhasePastTimeoutFails(t *testing.T) {
	svc, fl := newSvc(t)
	fl.phases = map[string]phaseInfo{"s1": {"", ""}}
	seed(t, svc, "r1", store.RunRunning, "s1", 21*time.Minute)
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if r := get(t, svc, "r1"); r.State != store.RunFailed || r.Outcome != "no finish within 20m0s" {
		t.Errorf("got %s %q", r.State, r.Outcome)
	}
}

func TestReconcileReapsTerminalAndUnknownSessions(t *testing.T) {
	svc, fl := newSvc(t)
	seed(t, svc, "done1", store.RunDone, "s-done", time.Hour)
	seed(t, svc, "stuck1", store.RunStuck, "s-stuck", time.Hour)
	seed(t, svc, "fail1", store.RunFailed, "s-fail", time.Hour)
	seed(t, svc, "live1", store.RunRunning, "s-live", time.Minute)
	fl.phases = map[string]phaseInfo{"s-live": {"Running", ""}}
	fl.sessions = map[string]string{
		"s-done": "done1", "s-stuck": "stuck1", "s-fail": "fail1",
		"s-live": "live1", "s-orphan": "nosuchrun",
	}
	if err := svc.Reconcile(context.Background(), t0, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range fl.stopped {
		got[s] = true
	}
	for _, want := range []string{"s-done", "s-stuck", "s-fail", "s-orphan"} {
		if !got[want] {
			t.Errorf("%s not reaped (stopped %v)", want, fl.stopped)
		}
	}
	if got["s-live"] {
		t.Errorf("active run's session was stopped")
	}
}
