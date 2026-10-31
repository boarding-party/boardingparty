package api

import (
	"context"
	"encoding/json"
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
func (noopLauncher) Stop(context.Context, string) error { return nil }

const projYAML = `name: p
sites: ["https://x.test"]
roles:
  - name: buyer
    personas: [{name: ann}]
    stories: [{name: checkout, goal: buy a thing}]
`

func setup(t *testing.T, maxBytes int64) (*httptest.Server, *runs.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &runs.Service{Store: st, Artifacts: artifacts.NewFS(t.TempDir(), maxBytes), Launcher: noopLauncher{}}
	srv := httptest.NewServer(New(svc, maxBytes))
	t.Cleanup(srv.Close)
	return srv, svc
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

func startRun(t *testing.T, srv *httptest.Server) store.Run {
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
	return run
}

func TestWorkerUploadNeedsActiveToken(t *testing.T) {
	srv, svc := setup(t, 1024)
	if r := do(t, "PUT", srv.URL+"/artifacts/deadbeef/a.png", "x"); r.StatusCode != 404 {
		t.Errorf("random token: %d", r.StatusCode)
	}
	run := startRun(t, srv)
	if r := do(t, "PUT", srv.URL+"/artifacts/"+run.Token+"/a.png", "x"); r.StatusCode != 201 {
		t.Errorf("active: %d", r.StatusCode)
	}
	if err := svc.Finish(context.Background(), run.Token, store.RunDone, "ok"); err != nil {
		t.Fatal(err)
	}
	if r := do(t, "PUT", srv.URL+"/artifacts/"+run.Token+"/b.png", "x"); r.StatusCode != 404 {
		t.Errorf("finished: %d", r.StatusCode)
	}
}

func TestUploadOversizeIs413(t *testing.T) {
	srv, _ := setup(t, 10)
	run := startRun(t, srv)
	if r := do(t, "PUT", srv.URL+"/artifacts/"+run.Token+"/a.png", strings.Repeat("x", 11)); r.StatusCode != 413 {
		t.Errorf("oversize: %d", r.StatusCode)
	}
}

func TestBadNameIs400(t *testing.T) {
	srv, _ := setup(t, 1024)
	run := startRun(t, srv)
	if r := do(t, "PUT", srv.URL+"/artifacts/"+run.Token+"/Bad", "x"); r.StatusCode != 400 {
		t.Errorf("bad name: %d", r.StatusCode)
	}
}

func TestRunsReadEndpoints(t *testing.T) {
	srv, _ := setup(t, 1024)
	run := startRun(t, srv)
	do(t, "PUT", srv.URL+"/artifacts/"+run.Token+"/a.txt", "hello")

	resp := do(t, "GET", srv.URL+"/api/runs", "")
	var list []store.Run
	json.NewDecoder(resp.Body).Decode(&list)
	if resp.StatusCode != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", resp.StatusCode, list)
	}

	resp = do(t, "GET", srv.URL+"/api/runs/"+run.ID, "")
	var detail struct {
		Run       store.Run
		Bugs      []store.Bug
		Artifacts []string
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
