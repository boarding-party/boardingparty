package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/runs"
	"github.com/tiny-systems/boardingparty/internal/store"
)

type noopLauncher struct{}

func (noopLauncher) Start(_ context.Context, r store.Run, _ project.Project) (string, error) {
	return "sess-" + r.ID, nil
}
func (noopLauncher) SessionName(id string) string       { return "sess-" + id }
func (noopLauncher) Stop(context.Context, string) error { return nil }
func (noopLauncher) Phase(context.Context, string) (string, bool, string, error) {
	return "Running", true, "", nil
}

func (noopLauncher) Sessions(context.Context) (map[string]string, error) { return nil, nil }
func (noopLauncher) LivePods(context.Context) (int, error)               { return 0, nil }

const projYAML = `name: p
sites: ["https://x.test"]
roles:
  - name: buyer
    personas: [{name: ann}]
    stories: [{name: checkout, goal: buy a thing}]
`

func setup(t *testing.T, maxBytes int64) (*httptest.Server, *runs.Service) {
	srv, _, svc := setupBoth(t, maxBytes)
	return srv, svc
}

// setupBoth returns the admin server, the worker server and the service.
func setupBoth(t *testing.T, maxBytes int64) (*httptest.Server, *httptest.Server, *runs.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &runs.Service{Store: st, Artifacts: artifacts.NewFS(t.TempDir(), maxBytes), Launcher: noopLauncher{}}
	srv := httptest.NewServer(Admin(svc, maxBytes))
	t.Cleanup(srv.Close)
	wsrv := httptest.NewServer(Worker(svc, maxBytes))
	t.Cleanup(wsrv.Close)
	return srv, wsrv, svc
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// startRun starts a run through the API and returns it with its token read
// from the store (the API response redacts it).
func startRun(t *testing.T, srv *httptest.Server, svc *runs.Service) store.Run {
	t.Helper()
	if r := do(t, "POST", srv.URL+"/api/project", projYAML); r.StatusCode != 204 {
		t.Fatalf("project: %d", r.StatusCode)
	}
	resp := do(t, "POST", srv.URL+"/api/runs", `{"persona":"ann","story":"checkout"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	full, err := svc.Store.RunByID(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

func TestWorkerUploadNeedsActiveToken(t *testing.T) {
	srv, w, svc := setupBoth(t, 1024)
	if r := do(t, "PUT", w.URL+"/artifacts/deadbeef/a.png", "x"); r.StatusCode != 404 {
		t.Errorf("random token: %d", r.StatusCode)
	}
	run := startRun(t, srv, svc)
	if r := do(t, "PUT", w.URL+"/artifacts/"+run.Token+"/a.png", "x"); r.StatusCode != 201 {
		t.Errorf("active: %d", r.StatusCode)
	}
	if err := svc.Finish(context.Background(), run.Token, store.RunDone, "ok"); err != nil {
		t.Fatal(err)
	}
	if r := do(t, "PUT", w.URL+"/artifacts/"+run.Token+"/b.png", "x"); r.StatusCode != 404 {
		t.Errorf("finished: %d", r.StatusCode)
	}
}

func TestUploadOversizeIs413(t *testing.T) {
	srv, w, svc := setupBoth(t, 10)
	run := startRun(t, srv, svc)
	if r := do(t, "PUT", w.URL+"/artifacts/"+run.Token+"/a.png", strings.Repeat("x", 11)); r.StatusCode != 413 {
		t.Errorf("oversize: %d", r.StatusCode)
	}
}

func TestBadNameIs400(t *testing.T) {
	srv, w, svc := setupBoth(t, 1024)
	run := startRun(t, srv, svc)
	if r := do(t, "PUT", w.URL+"/artifacts/"+run.Token+"/Bad", "x"); r.StatusCode != 400 {
		t.Errorf("bad name: %d", r.StatusCode)
	}
}

func TestRunsReadEndpoints(t *testing.T) {
	srv, w, svc := setupBoth(t, 1024)
	run := startRun(t, srv, svc)
	do(t, "PUT", w.URL+"/artifacts/"+run.Token+"/a.txt", "hello")

	resp := do(t, "GET", srv.URL+"/api/runs", "")
	var list []store.Run
	json.NewDecoder(resp.Body).Decode(&list)
	if resp.StatusCode != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", resp.StatusCode, list)
	}
	if list[0].Token != "" {
		t.Errorf("token leaked in list: %q", list[0].Token)
	}

	resp = do(t, "GET", srv.URL+"/api/runs/"+run.ID, "")
	var detail struct {
		Run       store.Run   `json:"run"`
		Bugs      []store.Bug `json:"bugs"`
		Artifacts []string    `json:"artifacts"`
	}
	json.NewDecoder(resp.Body).Decode(&detail)
	if resp.StatusCode != 200 || detail.Run.ID != run.ID || len(detail.Artifacts) != 1 || detail.Artifacts[0] != "a.txt" {
		t.Fatalf("detail: %d %+v", resp.StatusCode, detail)
	}

	resp = do(t, "GET", srv.URL+"/api/runs/"+run.ID+"/artifacts/a.txt", "")
	b := new(strings.Builder)
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	b.Write(buf[:n])
	if resp.StatusCode != 200 || b.String() != "hello" {
		t.Fatalf("artifact: %d %q", resp.StatusCode, b.String())
	}
	if r := do(t, "GET", srv.URL+"/api/runs/nope", ""); r.StatusCode != 404 {
		t.Errorf("missing run: %d", r.StatusCode)
	}
	if r := do(t, "POST", srv.URL+"/api/runs", `{"persona":"x","story":"y"}`); r.StatusCode != 400 {
		t.Errorf("unknown persona: %d", r.StatusCode)
	}
}

func TestAdminDoesNotServeWorkerUpload(t *testing.T) {
	srv, _, svc := setupBoth(t, 1024)
	run := startRun(t, srv, svc)
	if r := do(t, "PUT", srv.URL+"/artifacts/"+run.Token+"/a.png", "x"); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Errorf("admin served upload: %d", r.StatusCode)
	}
}

func TestWorkerServesNoAPI(t *testing.T) {
	srv, w, svc := setupBoth(t, 1024)
	run := startRun(t, srv, svc)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/project"}, {"POST", "/api/runs"}, {"GET", "/api/runs"},
		{"GET", "/api/runs/" + run.ID}, {"GET", "/api/runs/" + run.ID + "/artifacts/a.txt"},
	} {
		if r := do(t, c.method, w.URL+c.path, projYAML); r.StatusCode != 404 {
			t.Errorf("%s %s on worker: %d", c.method, c.path, r.StatusCode)
		}
	}
	if r := do(t, "GET", w.URL+"/healthz", ""); r.StatusCode != 200 {
		t.Errorf("healthz: %d", r.StatusCode)
	}
}

func TestStartRunLauncherErrorIsGeneric(t *testing.T) {
	srv, svc := setup(t, 1024)
	svc.Launcher = failLauncher{}
	do(t, "POST", srv.URL+"/api/project", projYAML)
	resp := do(t, "POST", srv.URL+"/api/runs", `{"persona":"ann","story":"checkout"}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 502 || strings.Contains(string(b), "secret-detail") {
		t.Errorf("got %d %q", resp.StatusCode, b)
	}
}

type failLauncher struct{ noopLauncher }

func (failLauncher) Start(context.Context, store.Run, project.Project) (string, error) {
	return "", errors.New("secret-detail")
}

func TestStartRunBusyIs409AndTokenRedacted(t *testing.T) {
	srv, _ := setup(t, 1024)
	if r := do(t, "POST", srv.URL+"/api/project", projYAML); r.StatusCode != 204 {
		t.Fatalf("project: %d", r.StatusCode)
	}
	resp := do(t, "POST", srv.URL+"/api/runs", `{"persona":"ann","story":"checkout"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var run store.Run
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	if run.Token != "" {
		t.Errorf("start response leaks token: %s", b)
	}
	resp = do(t, "POST", srv.URL+"/api/runs", `{"persona":"ann","story":"checkout"}`)
	if resp.StatusCode != 409 {
		t.Fatalf("second start: %d, want 409", resp.StatusCode)
	}
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "another run is active") {
		t.Errorf("message: %s", b)
	}
}

type podErrLauncher struct{ noopLauncher }

func (podErrLauncher) LivePods(context.Context) (int, error) { return 0, errors.New("secret-detail") }

func TestStartRunInternalErrorIs500NotRawText(t *testing.T) {
	srv, svc := setup(t, 1024)
	svc.Launcher = podErrLauncher{}
	do(t, "POST", srv.URL+"/api/project", projYAML)
	resp := do(t, "POST", srv.URL+"/api/runs", `{"persona":"ann","story":"checkout"}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || strings.Contains(string(b), "secret-detail") {
		t.Errorf("got %d %q", resp.StatusCode, b)
	}
}

func TestStartRunUnknownPersonaIs400(t *testing.T) {
	srv, _ := setup(t, 1024)
	do(t, "POST", srv.URL+"/api/project", projYAML)
	resp := do(t, "POST", srv.URL+"/api/runs", `{"persona":"nobody","story":"checkout"}`)
	if resp.StatusCode != 400 {
		t.Errorf("got %d, want 400", resp.StatusCode)
	}
}
