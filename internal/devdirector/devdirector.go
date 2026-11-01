// Package devdirector runs a director admin API in this process with a fake
// launcher, so the board UI can be checked without a cluster. Runs never touch
// Kubernetes: a started run goes to running, then finishes after a delay with
// a sample recording and one bug.
package devdirector

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/boarding-party/boardingparty/internal/api"
	"github.com/boarding-party/boardingparty/internal/artifacts"
	"github.com/boarding-party/boardingparty/internal/project"
	"github.com/boarding-party/boardingparty/internal/runs"
	"github.com/boarding-party/boardingparty/internal/store"
)

//go:embed testdata/sample.webm
var sampleWebm []byte

//go:embed testdata/todomvc.yaml
var sampleProject []byte

// SampleProject is the project a dev director starts with (a copy of
// examples/todomvc.yaml; a test keeps them equal).
func SampleProject() []byte { return append([]byte(nil), sampleProject...) }

// RunDuration is how long a fake run takes before it finishes.
const RunDuration = 20 * time.Second

// SampleArtifact is the name of the recording a fake run uploads.
const SampleArtifact = "run.webm"

// Launcher is a runs.Launcher with no cluster behind it. A started run
// finishes "done" after Duration with a sample recording and one bug.
type Launcher struct {
	Duration time.Duration
	Svc      *runs.Service // set after the Service is built

	mu       sync.Mutex
	sessions map[string]string // session -> run ID
}

func (l *Launcher) SessionName(runID string) string { return "fake-" + runID }

func (l *Launcher) Start(ctx context.Context, r store.Run, _ project.Project) (string, error) {
	name := l.SessionName(r.ID)
	l.mu.Lock()
	if l.sessions == nil {
		l.sessions = map[string]string{}
	}
	l.sessions[name] = r.ID
	l.mu.Unlock()
	go l.finishLater(r)
	return name, nil
}

func (l *Launcher) finishLater(r store.Run) {
	time.Sleep(l.Duration)
	if _, err := l.Svc.Active(r.Token); err != nil {
		return // already ended
	}
	if _, err := l.Svc.Artifacts.Put(r.ID, SampleArtifact, bytes.NewReader(sampleWebm)); err != nil {
		_ = l.Svc.Finish(context.Background(), r.Token, store.RunFailed, "fake run: "+err.Error())
		return
	}
	_ = l.Svc.Store.AddBug(store.Bug{
		ID: "bug-" + r.ID, RunID: r.ID,
		Summary:  "Counter still says 3 items left after completing one",
		Step:     "00:02 ticked the first todo; the counter did not change",
		Evidence: "the footer reads \"3 items left\" with one item struck through",
		Severity: "medium",
		Note:     "I expected 2 items left, so I stopped trusting the count.",
		Created:  time.Now().UTC(),
	})
	_ = l.Svc.Finish(context.Background(), r.Token, store.RunDone, "Added three todos and completed one. The counter did not update (fake run).")
}

func (l *Launcher) Stop(_ context.Context, session string) error {
	l.mu.Lock()
	delete(l.sessions, session)
	l.mu.Unlock()
	return nil
}

func (l *Launcher) Phase(_ context.Context, session string) (string, bool, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.sessions[session]; !ok {
		return "", false, "", nil
	}
	return "Running", true, "", nil
}

func (l *Launcher) Sessions(_ context.Context) (map[string]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.sessions))
	for k, v := range l.sessions {
		out[k] = v
	}
	return out, nil
}

func (l *Launcher) LivePods(context.Context) (int, error) { return 0, nil }

// Director is a running in-process director admin API.
type Director struct {
	Base  string // http://127.0.0.1:<port>
	Token string // bearer secret the admin API requires

	srv *http.Server
	st  *store.Store
	dir string
}

// Start serves the admin API on a loopback port, with a temp data directory,
// the project loaded from projectYAML, and a fake launcher taking duration per run.
// Call Close to stop it and delete the data.
func Start(ctx context.Context, projectYAML []byte, duration time.Duration) (*Director, error) {
	dir, err := os.MkdirTemp("", "bp-dev-director-")
	if err != nil {
		return nil, err
	}
	d, err := start(ctx, dir, projectYAML, duration)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return d, nil
}

func start(ctx context.Context, dir string, projectYAML []byte, duration time.Duration) (*Director, error) {
	st, err := store.Open(filepath.Join(dir, "boardingparty.db"))
	if err != nil {
		return nil, err
	}
	p, err := project.Load(projectYAML)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("sample project: %w", err)
	}
	if err := st.PutProject(p); err != nil {
		st.Close()
		return nil, err
	}
	l := &Launcher{Duration: duration}
	svc := &runs.Service{
		Store:     st,
		Artifacts: artifacts.NewFS(filepath.Join(dir, "artifacts"), api.MaxArtifactBytes),
		Launcher:  l,
	}
	l.Svc = svc
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		st.Close()
		return nil, err
	}
	token := hex.EncodeToString(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		st.Close()
		return nil, err
	}
	srv := &http.Server{Handler: api.Admin(svc, 0, token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "dev director:", err)
		}
	}()
	go svc.Watch(ctx, time.Second, 10*time.Minute)
	return &Director{Base: "http://" + ln.Addr().String(), Token: token, srv: srv, st: st, dir: dir}, nil
}

// Close stops the server and removes the temp data.
func (d *Director) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if d.srv.Shutdown(ctx) != nil {
		d.srv.Close()
	}
	d.st.Close()
	os.RemoveAll(d.dir)
}

// Forwarder is a board.Forwarder that always points at the in-process server.
type Forwarder struct{ Base string }

func (f Forwarder) Open(context.Context) (string, error) { return f.Base, nil }
func (f Forwarder) Close()                               {}
