package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type stubForwarder struct {
	mu            sync.Mutex
	base          string
	err           error
	opens, closes int
}

func (f *stubForwarder) Open(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	return f.base, f.err
}

func (f *stubForwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
}

func TestBoardServesUntilCancelled(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"r1"}]`)
	}))
	defer dir.Close()
	var opened []string
	defer func(f func(string) error) { openBrowser = f }(openBrowser)
	openBrowser = func(u string) error { opened = append(opened, u); return errors.New("no browser here") }

	fw := &stubForwarder{base: dir.URL}
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- runBoard(ctx, fw, boardOpts{}, out) }()

	re := regexp.MustCompile(`→ (http://127\.0\.0\.1:\d+/\?key=([0-9a-f]{64}))\n`)
	var m []string
	for deadline := time.Now().Add(5 * time.Second); m == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		m = re.FindStringSubmatch(out.String())
	}
	if m == nil {
		cancel()
		t.Fatalf("no URL printed:\n%s", out.String())
	}
	if !strings.HasPrefix(out.String(), "✓ director ready\n") {
		t.Errorf("output = %q", out.String())
	}
	if !strings.Contains(out.String(), "couldn't open a browser") {
		t.Errorf("browser failure not reported gently: %q", out.String())
	}
	if len(opened) != 1 || opened[0] != m[1] {
		t.Errorf("opened = %v", opened)
	}
	if strings.Count(out.String(), m[2]) != 1 {
		t.Errorf("token printed more than once")
	}

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Get(m[1])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.RawQuery != "" {
		t.Errorf("first load = %d at %s", resp.StatusCode, resp.Request.URL)
	}
	base := strings.SplitN(m[1], "/?", 2)[0]
	resp, err = c.Get(base + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != `[{"id":"r1"}]` {
		t.Errorf("api = %d %q", resp.StatusCode, b)
	}
	resp, err = http.Get(base + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no cookie = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runBoard = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runBoard didn't stop")
	}
	if fw.closes == 0 {
		t.Errorf("forward not closed")
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Errorf("server still listening")
	}
}

func TestBoardNoOpen(t *testing.T) {
	defer func(f func(string) error) { openBrowser = f }(openBrowser)
	openBrowser = func(string) error { t.Error("browser opened with --no-open"); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- runBoard(ctx, &stubForwarder{base: "http://127.0.0.1:1"}, boardOpts{noOpen: true}, out)
	}()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "→ ") && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBoardForwardFailureIsAnError(t *testing.T) {
	out := &syncBuf{}
	err := runBoard(context.Background(), &stubForwarder{err: errors.New("no ready director pod")}, boardOpts{}, out)
	if err == nil || !strings.Contains(err.Error(), "no ready director pod") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(out.String(), "→") {
		t.Errorf("printed a URL without a director: %q", out.String())
	}
}
