package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

func TestRunsTableFormatting(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	formatRuns(&buf, []Run{
		{ID: "r1", Token: "SECRET", Persona: "alice", Story: "signup", State: "done", Started: now.Add(-5 * time.Minute), Outcome: "all good"},
		{ID: "r2", Persona: "bob", Story: "checkout", State: "running", Started: now.Add(-30 * time.Second)},
	}, now)
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d:\n%s", len(lines), out)
	}
	for _, h := range []string{"ID", "PERSONA", "STORY", "STATE", "AGE", "OUTCOME"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header missing %s", h)
		}
	}
	if f := strings.Fields(lines[1]); f[0] != "r1" || f[1] != "alice" || f[3] != "done" || f[4] != "5m" {
		t.Errorf("row1 = %v", f)
	}
	if !strings.Contains(lines[2], "30s") {
		t.Errorf("row2 = %q", lines[2])
	}
	if strings.Contains(out, "SECRET") {
		t.Error("token printed")
	}
}

func artifactServer(t *testing.T, names []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RunDetail{Run: Run{ID: r.PathValue("id"), State: "done"}, Artifacts: names})
	})
	mux.HandleFunc("GET /api/runs/{id}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("content of " + r.PathValue("name")))
	})
	return httptest.NewServer(mux)
}

func TestGetDownloadsArtifacts(t *testing.T) {
	srv := artifactServer(t, []string{"shot.png", "trace.zip"})
	defer srv.Close()
	c := newClient(srv.URL)
	d, err := c.RunDetail(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	if err := downloadArtifacts(context.Background(), c, d, dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "trace.zip"))
	if err != nil || string(b) != "content of trace.zip" {
		t.Fatalf("got %q, %v", b, err)
	}
}

func TestGetRejectsPathSeparators(t *testing.T) {
	for _, bad := range []string{"../evil", "a/b", `a\b`, ".."} {
		srv := artifactServer(t, []string{"ok.txt", bad})
		c := newClient(srv.URL)
		d, _ := c.RunDetail(context.Background(), "r1")
		dir := filepath.Join(t.TempDir(), "out")
		if err := downloadArtifacts(context.Background(), c, d, dir); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err == nil {
			t.Errorf("%q: wrote files before validating", bad)
		}
		srv.Close()
	}
}

func TestRunWaitExitCodes(t *testing.T) {
	setPoll(t)
	for _, tc := range []struct {
		state string
		code  int
	}{{"done", 0}, {"stuck", 1}, {"failed", 1}} {
		var polls atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Run{ID: "r1", Token: "tok", State: "running"})
		})
		mux.HandleFunc("GET /api/runs/r1", func(w http.ResponseWriter, r *http.Request) {
			st := "running"
			if polls.Add(1) >= 3 {
				st = tc.state
			}
			json.NewEncoder(w).Encode(RunDetail{Run: Run{ID: "r1", State: st, Outcome: "because"}})
		})
		srv := httptest.NewServer(mux)
		var out bytes.Buffer
		err := runAndWait(context.Background(), newClient(srv.URL), &out, "alice", "signup", true, time.Minute)
		srv.Close()
		code := 0
		var ee ExitError
		if errors.As(err, &ee) {
			code = ee.Code
		} else if err != nil {
			t.Fatalf("%s: %v", tc.state, err)
		}
		if code != tc.code {
			t.Errorf("%s: code %d want %d", tc.state, code, tc.code)
		}
		if !strings.Contains(out.String(), "because") {
			t.Errorf("%s: outcome not printed: %s", tc.state, out.String())
		}
	}
}

func TestRunWaitTimeout(t *testing.T) {
	setPoll(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(Run{ID: "r1"}) })
	mux.HandleFunc("GET /api/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RunDetail{Run: Run{ID: "r1", State: "running"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	err := runAndWait(context.Background(), newClient(srv.URL), &bytes.Buffer{}, "a", "b", true, 20*time.Millisecond)
	if err == nil || errors.As(err, new(ExitError)) {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestTokenOnlyInRunOutput(t *testing.T) {
	var buf bytes.Buffer
	printDetail(&buf, RunDetail{Run: Run{ID: "r1", Token: "SECRET"}})
	if strings.Contains(buf.String(), "SECRET") {
		t.Error("get leaks token")
	}
}

func setPoll(t *testing.T) {
	old := pollEvery
	pollEvery = time.Millisecond
	t.Cleanup(func() { pollEvery = old })
}

func TestOutputStripsControlChars(t *testing.T) {
	evil := "x\x1b[2J\u009b31m\x07y\tz"
	var buf bytes.Buffer
	printDetail(&buf, RunDetail{
		Run:       Run{ID: "r1", Persona: evil, Story: evil, State: "done", Outcome: evil},
		Bugs:      []Bug{{Summary: evil, Step: evil, Evidence: evil, Note: evil, Severity: evil}},
		Artifacts: []string{evil},
	})
	formatRuns(&buf, []Run{{ID: evil, Persona: evil, Story: evil, State: evil, Outcome: evil}}, time.Now())
	for _, r := range buf.String() {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			t.Fatalf("control rune %q in output", r)
		}
	}
	if strings.ContainsAny(buf.String(), "\x1b\x9b") {
		t.Error("raw ESC/CSI byte in output")
	}
	if !strings.Contains(buf.String(), "x?[2J?31m?y\tz") {
		t.Errorf("unexpected cleaning: %q", buf.String())
	}
}

func TestWaitToleratesTransientErrors(t *testing.T) {
	setPoll(t)
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(Run{ID: "r1"}) })
	mux.HandleFunc("GET /api/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) <= 2 {
			http.Error(w, "boom \x1b[2J", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(RunDetail{Run: Run{ID: "r1", State: "done"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := runAndWait(context.Background(), newClient(srv.URL), &bytes.Buffer{}, "a", "b", true, time.Minute); err != nil {
		t.Fatalf("want exit 0, got %v", err)
	}
}

func TestWaitGivesUpAfterThreeConsecutiveErrors(t *testing.T) {
	setPoll(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(Run{ID: "r1"}) })
	mux.HandleFunc("GET /api/runs/r1", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 502) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	err := runAndWait(context.Background(), newClient(srv.URL), &bytes.Buffer{}, "a", "b", true, time.Minute)
	if err == nil || errors.As(err, new(ExitError)) {
		t.Fatalf("want poll error, got %v", err)
	}
}

func runWithPolls(t *testing.T, script []string) error {
	t.Helper()
	setPoll(t)
	var n atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(Run{ID: "r1"}) })
	mux.HandleFunc("GET /api/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		step := "done"
		if i < len(script) {
			step = script[i]
		}
		if step == "fail" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(RunDetail{Run: Run{ID: "r1", State: step}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	return runAndWait(context.Background(), newClient(srv.URL), &bytes.Buffer{}, "a", "b", true, time.Minute)
}

func TestPollToleranceBoundary(t *testing.T) {
	if err := runWithPolls(t, []string{"fail", "fail", "fail", "done"}); err != nil {
		t.Errorf("3 failures then done: %v", err)
	}
	if err := runWithPolls(t, []string{"fail", "fail", "fail", "fail", "done"}); err == nil {
		t.Error("4 failures should error")
	}
}

func TestPollErrorCountResets(t *testing.T) {
	if err := runWithPolls(t, []string{"fail", "fail", "running", "fail", "fail", "done"}); err != nil {
		t.Errorf("reset: %v", err)
	}
}

func TestErrorTextIsSanitised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad \x1b[31mred\u009b2J", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Runs(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	got := FormatError(err)
	if strings.ContainsAny(got, "\x1b\x9b") || strings.ContainsRune(got, '\u009b') {
		t.Errorf("control chars in %q", got)
	}
	if !strings.Contains(got, "bad ?[31mred?2J") {
		t.Errorf("got %q", got)
	}
}

func TestRunsTableTabsDoNotShiftColumns(t *testing.T) {
	var buf bytes.Buffer
	formatRuns(&buf, []Run{{ID: "r1", Persona: "a\tb", Story: "s", State: "done", Outcome: "x\ty"}}, time.Now())
	if strings.Count(strings.Split(buf.String(), "\n")[1], "\t") != 0 {
		t.Errorf("tab leaked: %q", buf.String())
	}
}

func TestCleanStripsFormatChars(t *testing.T) {
	in := "a\u202Eb\u200Bc\u2066d\te"
	if got, want := clean(in), "a?b?c?d\te"; got != want {
		t.Errorf("clean = %q, want %q", got, want)
	}
}
