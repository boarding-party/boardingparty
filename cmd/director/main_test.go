package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/runs"
	"github.com/tiny-systems/boardingparty/internal/store"
)

type fakeLauncher struct{}

func (fakeLauncher) Start(_ context.Context, r store.Run, _ project.Project) (string, error) {
	return "sess-" + r.ID, nil
}
func (fakeLauncher) SessionName(id string) string       { return "sess-" + id }
func (fakeLauncher) Stop(context.Context, string) error { return nil }
func (fakeLauncher) Phase(context.Context, string) (string, bool, string, error) {
	return "Running", true, "", nil
}

func (fakeLauncher) Sessions(context.Context) (map[string]string, error) { return nil, nil }

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServerRoutes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config{DirectorURL: "http://bp-director:8080", MaxArtifactBytes: 1 << 20}
	svc := &runs.Service{Store: st, Artifacts: artifacts.NewFS(t.TempDir(), cfg.MaxArtifactBytes), Launcher: fakeLauncher{}}
	admin, worker := newServers(cfg, svc, st)

	// worker listener: health yes, operator API no.
	if c := do(t, worker, "GET", "/healthz", "").Code; c != 200 {
		t.Fatalf("worker /healthz = %d", c)
	}
	if c := do(t, worker, "POST", "/api/runs", `{}`).Code; c != 404 {
		t.Fatalf("worker POST /api/runs = %d, want 404", c)
	}
	if c := do(t, worker, "POST", "/api/project", `x`).Code; c != 404 {
		t.Fatalf("worker POST /api/project = %d, want 404", c)
	}
	if c := do(t, worker, "POST", "/mcp/run/nosuchtoken", `{}`).Code; c == 404 {
		t.Fatalf("worker /mcp/run/ not mounted")
	}

	// admin listener: operator API.
	y, err := os.ReadFile("../../examples/todomvc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c := do(t, admin, "POST", "/api/project", string(y)).Code; c != 204 {
		t.Fatalf("admin POST /api/project = %d", c)
	}
	p, err := project.Load(y)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"persona":"` + p.Roles[0].Personas[0].Name + `","story":"` + p.Roles[0].Stories[0].Name + `"}`
	if rec := do(t, admin, "POST", "/api/runs", body); rec.Code != 201 {
		t.Fatalf("admin POST /api/runs = %d %s", rec.Code, rec.Body)
	}
}

func TestDefaultDirectorURLIsSvcName(t *testing.T) {
	got := defaultDirectorURL("bp-todomvc")
	if got != "http://bp-director.bp-todomvc.svc:8080" {
		t.Errorf("got %q", got)
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	good := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	busy := &http.Server{Addr: ln.Addr().String(), Handler: http.NotFoundHandler()}
	if err := serve(context.Background(), []*http.Server{good, busy}); err == nil {
		t.Fatal("serve returned nil despite a listener failure")
	}
}
