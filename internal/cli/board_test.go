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

func (f *stubForwarder) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

var linkRE = regexp.MustCompile(`→ (http://127\.0\.0\.1:\d+/\?key=([0-9a-f]{64}))\n`)

// waitFor polls out until it matches re n times.
func waitFor(t *testing.T, out *syncBuf, n int) [][]string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if m := linkRE.FindAllStringSubmatch(out.String(), -1); len(m) >= n {
			return m
		}
	}
	t.Fatalf("wanted %d links, got:\n%s", n, out.String())
	return nil
}

func stubBrowser(t *testing.T, err error) <-chan string {
	t.Helper()
	ch := make(chan string, 4)
	old := openBrowser
	openBrowser = func(u string) error { ch <- u; return err }
	t.Cleanup(func() { openBrowser = old })
	return ch
}

func TestBoardServesUntilCancelled(t *testing.T) {
	var gotAuth string
	var mu sync.Mutex
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		io.WriteString(w, `[{"id":"r1"}]`)
	}))
	defer dir.Close()
	opened := stubBrowser(t, errors.New("no browser here"))

	fw := &stubForwarder{base: dir.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- runBoard(ctx, fw, boardOpts{adminToken: "adm"}, out) }()

	m := waitFor(t, out, 1)[0]
	select {
	case u := <-opened:
		if u != m[1] {
			t.Errorf("opened %q, printed %q", u, m[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("browser not opened")
	}
	// The failure note is written right after openBrowser returns.
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "couldn't open a browser") && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "couldn't open a browser") {
		t.Errorf("browser failure not reported gently: %q", out.String())
	}
	if !strings.HasPrefix(out.String(), "✓ director ready\n") {
		t.Errorf("output = %q", out.String())
	}
	if strings.Contains(out.String(), "press Enter") {
		t.Errorf("Enter hint without a terminal: %q", out.String())
	}
	if strings.Count(out.String(), m[2]) != 1 || strings.Contains(out.String(), "adm") {
		t.Errorf("secret printed: %q", out.String())
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
	mu.Lock()
	if gotAuth != "Bearer adm" {
		t.Errorf("director saw Authorization %q", gotAuth)
	}
	mu.Unlock()
	// No cookie: 401. The used link in a second browser: 401 too.
	for _, u := range []string{base + "/api/runs", m[1]} {
		resp, err = http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without session = %d", u, resp.StatusCode)
		}
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
	if fw.closeCount() == 0 {
		t.Errorf("forward not closed")
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Errorf("server still listening")
	}
}

func TestBoardEnterGivesANewLink(t *testing.T) {
	stubBrowser(t, nil)
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- runBoard(ctx, &stubForwarder{base: "http://127.0.0.1:1"}, boardOpts{noOpen: true, in: pr}, out)
	}()
	first := waitFor(t, out, 1)[0]
	if !strings.Contains(out.String(), "press Enter for a new link") {
		t.Errorf("no hint: %q", out.String())
	}
	// Using the first link prints nothing new.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(first[1])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first link = %d", resp.StatusCode)
	}
	if n := len(linkRE.FindAllString(out.String(), -1)); n != 1 {
		t.Errorf("links after use = %d", n)
	}
	if _, err := pw.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	second := waitFor(t, out, 2)[1]
	if second[2] == first[2] {
		t.Fatal("same key twice")
	}
	jar, _ := cookiejar.New(nil)
	resp, err = (&http.Client{Jar: jar}).Get(second[1])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("second link = %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBoardNoOpen(t *testing.T) {
	opened := stubBrowser(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- runBoard(ctx, &stubForwarder{base: "http://127.0.0.1:1"}, boardOpts{noOpen: true}, out)
	}()
	waitFor(t, out, 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-opened:
		t.Errorf("browser opened with --no-open: %s", u)
	default:
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
