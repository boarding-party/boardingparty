package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/runs"
	"github.com/tiny-systems/boardingparty/internal/store"
)

type fakeLauncher struct{ mu sync.Mutex }

func (f *fakeLauncher) Start(_ context.Context, r store.Run, _ project.Project) (string, error) {
	return "sess-" + r.ID, nil
}
func (f *fakeLauncher) SessionName(id string) string       { return "sess-" + id }
func (f *fakeLauncher) Stop(context.Context, string) error { return nil }
func (f *fakeLauncher) Phase(context.Context, string) (string, bool, string, error) {
	return "Running", true, "", nil
}

func (f *fakeLauncher) Sessions(context.Context) (map[string]string, error) { return nil, nil }

const director = "http://director.test"

func setup(t *testing.T) (*runs.Service, *store.Store, *httptest.Server, store.Run) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := project.Project{Name: "p", Sites: []string{"https://x.test"}, Roles: []project.Role{{
		Name: "buyer",
		Personas: []project.Persona{{Name: "ann", Traits: []string{"impatient"},
			Identity: map[string]string{"email": "ann@example.test"}}},
		Stories: []project.Story{{Name: "checkout", Goal: "buy a thing"}},
	}}}
	if err := st.PutProject(p); err != nil {
		t.Fatal(err)
	}
	svc := &runs.Service{Store: st, Artifacts: artifacts.NewFS(t.TempDir(), 1024), Launcher: &fakeLauncher{}}
	run, err := svc.Start(context.Background(), "ann", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(svc, st, director))
	t.Cleanup(srv.Close)
	return svc, st, srv, run
}

func connect(t *testing.T, srv *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp/run/" + token}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func TestNextAssignmentReturnsPersonaAndStory(t *testing.T) {
	_, st, srv, run := setup(t)
	if err := st.SavePersonaState("ann", json.RawMessage(`{"seen":1}`)); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, srv, run.Token)
	text, isErr := call(t, cs, "next_assignment", nil)
	if isErr {
		t.Fatalf("error: %s", text)
	}
	var got struct {
		Persona struct {
			Name     string            `json:"name"`
			Role     string            `json:"role"`
			Traits   []string          `json:"traits"`
			Identity map[string]string `json:"identity"`
		} `json:"persona"`
		Story struct {
			Name string `json:"name"`
			Goal string `json:"goal"`
		} `json:"story"`
		Sites        []string        `json:"sites"`
		PersonaState json.RawMessage `json:"personaState"`
		UploadURL    string          `json:"uploadURL"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	if got.Persona.Name != "ann" || got.Persona.Identity["email"] != "ann@example.test" || got.Persona.Traits[0] != "impatient" {
		t.Errorf("persona: %+v", got.Persona)
	}
	if got.Story.Name != "checkout" || got.Story.Goal != "buy a thing" {
		t.Errorf("story: %+v", got.Story)
	}
	if len(got.Sites) != 1 || got.Sites[0] != "https://x.test" {
		t.Errorf("sites: %v", got.Sites)
	}
	if string(got.PersonaState) != `{"seen":1}` {
		t.Errorf("state: %s", got.PersonaState)
	}
	if want := director + "/artifacts/" + run.Token + "/"; got.UploadURL != want {
		t.Errorf("uploadURL %q want %q", got.UploadURL, want)
	}

	if text, isErr := call(t, cs, "persona_save", map[string]any{"state": map[string]any{"x": 2}}); isErr || text != "saved" {
		t.Fatalf("persona_save: %q %v", text, isErr)
	}
	s, _ := st.PersonaState("ann")
	if string(s) != `{"x":2}` {
		t.Errorf("stored state %s", s)
	}
}

func TestUnknownTokenEveryToolErrors(t *testing.T) {
	_, _, srv, _ := setup(t)
	cs := connect(t, srv, "nope")
	calls := map[string]map[string]any{
		"next_assignment": nil,
		"persona_save":    {"state": map[string]any{}},
		"report_bug":      {"summary": "s", "step": "st", "evidence": "e", "severity_guess": "low", "note": "n"},
		"finish_run":      {"outcome": "done", "summary": "x"},
	}
	for name, args := range calls {
		text, isErr := call(t, cs, name, args)
		if !isErr || text != "unknown or finished run" {
			t.Errorf("%s: isErr=%v text=%q", name, isErr, text)
		}
	}
}

func TestFinishThenToolsError(t *testing.T) {
	_, st, srv, run := setup(t)
	cs := connect(t, srv, run.Token)
	if text, isErr := call(t, cs, "finish_run", map[string]any{"outcome": "stuck", "summary": "blocked"}); isErr || text != "finished" {
		t.Fatalf("finish: %q %v", text, isErr)
	}
	r, _ := st.RunByID(run.ID)
	if r.State != store.RunStuck || r.Outcome != "blocked" {
		t.Errorf("run: %+v", r)
	}
	for name, args := range map[string]map[string]any{"next_assignment": nil, "finish_run": {"outcome": "done", "summary": "x"}} {
		text, isErr := call(t, cs, name, args)
		if !isErr || text != "unknown or finished run" {
			t.Errorf("%s: %v %q", name, isErr, text)
		}
	}
}

func TestReportBugStored(t *testing.T) {
	_, st, srv, run := setup(t)
	cs := connect(t, srv, run.Token)
	text, isErr := call(t, cs, "report_bug", map[string]any{
		"summary": "button dead", "step": "3", "evidence": "shot.png", "severity_guess": "high", "note": "n"})
	if isErr || text == "" {
		t.Fatalf("report_bug: %q %v", text, isErr)
	}
	bugs, err := st.Bugs(run.ID)
	if err != nil || len(bugs) != 1 {
		t.Fatalf("bugs %v %v", bugs, err)
	}
	b := bugs[0]
	if b.ID != text || b.Summary != "button dead" || b.Severity != "high" || b.Evidence != "shot.png" || b.Step != "3" {
		t.Errorf("bug %+v (id returned %q)", b, text)
	}
	if text, isErr := call(t, cs, "report_bug", map[string]any{"summary": "s", "severity_guess": "critical"}); !isErr {
		t.Errorf("bad severity accepted: %q", text)
	}
}

func TestFinishRejectsBadOutcome(t *testing.T) {
	_, st, srv, run := setup(t)
	cs := connect(t, srv, run.Token)
	if text, isErr := call(t, cs, "finish_run", map[string]any{"outcome": "pending", "summary": "x"}); !isErr {
		t.Fatalf("accepted: %q", text)
	}
	r, _ := st.RunByID(run.ID)
	if r.State != store.RunRunning {
		t.Errorf("state %s", r.State)
	}
}

// Two active runs for two personas: each token only ever touches its own
// persona state, bugs and run state. Runs are created in the store directly
// because Service.Start allows only one active run at a time.
func TestCrossRunIsolation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := project.Project{Name: "p", Sites: []string{"https://x.test"}, Roles: []project.Role{{
		Name:     "buyer",
		Personas: []project.Persona{{Name: "ann"}, {Name: "bob"}},
		Stories:  []project.Story{{Name: "checkout", Goal: "buy a thing"}},
	}}}
	if err := st.PutProject(p); err != nil {
		t.Fatal(err)
	}
	svc := &runs.Service{Store: st, Artifacts: artifacts.NewFS(t.TempDir(), 1024), Launcher: &fakeLauncher{}}
	mk := func(id, token, persona string) store.Run {
		r := store.Run{ID: id, Token: token, Persona: persona, Role: "buyer", Story: "checkout",
			State: store.RunRunning, Session: "sess-" + id, Started: time.Now().UTC()}
		if err := st.CreateRun(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := mk("run-a", "token-a", "ann"), mk("run-b", "token-b", "bob")
	if err := st.SavePersonaState("bob", json.RawMessage(`{"owner":"bob"}`)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(svc, st, director))
	t.Cleanup(srv.Close)

	cs := connect(t, srv, a.Token)
	if text, isErr := call(t, cs, "persona_save", map[string]any{"state": map[string]any{"owner": "ann"}}); isErr {
		t.Fatalf("persona_save: %s", text)
	}
	if text, isErr := call(t, cs, "report_bug", map[string]any{
		"summary": "bug for A", "step": "1", "evidence": "e", "severity_guess": "low", "note": "n"}); isErr {
		t.Fatalf("report_bug: %s", text)
	}
	if text, isErr := call(t, cs, "finish_run", map[string]any{"outcome": "done", "summary": "A done"}); isErr {
		t.Fatalf("finish_run: %s", text)
	}

	if s, _ := st.PersonaState("ann"); string(s) != `{"owner":"ann"}` {
		t.Errorf("ann state = %s", s)
	}
	if s, _ := st.PersonaState("bob"); string(s) != `{"owner":"bob"}` {
		t.Errorf("bob state touched by run A: %s", s)
	}
	if bugs, _ := st.Bugs(b.ID); len(bugs) != 0 {
		t.Errorf("run B has bugs from run A: %+v", bugs)
	}
	if bugs, _ := st.Bugs(a.ID); len(bugs) != 1 {
		t.Errorf("run A bugs = %+v", bugs)
	}
	ra, _ := st.RunByID(a.ID)
	rb, _ := st.RunByID(b.ID)
	if ra.State != store.RunDone {
		t.Errorf("run A state %s", ra.State)
	}
	if rb.State != store.RunRunning || rb.Outcome != "" {
		t.Errorf("run B affected: %+v", rb)
	}
}
