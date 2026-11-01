package devdirector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boarding-party/boardingparty/internal/store"
)

func sample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../examples/todomvc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, SampleProject()) {
		t.Fatal("testdata/todomvc.yaml differs from examples/todomvc.yaml; copy it over")
	}
	return b
}

func do(t *testing.T, d *Director, method, path, ctype, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, d.Base+path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestRunLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Start(ctx, sample(t), 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if resp, _ := do(t, d, "GET", "/api/project", "", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", resp.StatusCode)
	}
	if resp, _ := do(t, d, "GET", "/api/project", "", d.Token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("project: got %d", resp.StatusCode)
	}
	resp, b := do(t, d, "POST", "/api/runs", "application/json", d.Token, `{"persona":"Ada","story":"todos"}`)
	if resp.StatusCode >= 300 {
		t.Fatalf("start: %d %s", resp.StatusCode, b)
	}
	var run store.Run
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	if run.State != store.RunRunning {
		t.Fatalf("state after start = %q, want running", run.State)
	}
	// A second run while one is active is refused.
	if resp, _ := do(t, d, "POST", "/api/runs", "application/json", d.Token, `{"persona":"Ada","story":"todos"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second start: got %d, want 409", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := d.st.RunByID(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == store.RunDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run still %q after 5s", got.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	bugs, err := d.st.Bugs(run.ID)
	if err != nil || len(bugs) != 1 || !strings.Contains(bugs[0].Step, "00:02") {
		t.Fatalf("bugs = %+v, %v", bugs, err)
	}
	resp, vid := do(t, d, "GET", "/api/runs/"+run.ID+"/artifacts/"+SampleArtifact, "", d.Token, "")
	if resp.StatusCode != http.StatusOK || len(vid) != len(sampleWebm) {
		t.Fatalf("artifact: %d, %d bytes (want %d)", resp.StatusCode, len(vid), len(sampleWebm))
	}
	// The session is gone, so the next run can start.
	if resp, _ := do(t, d, "POST", "/api/runs", "application/json", d.Token, `{"persona":"Ada","story":"todos"}`); resp.StatusCode >= 300 {
		t.Fatalf("start after finish: got %d", resp.StatusCode)
	}
}
