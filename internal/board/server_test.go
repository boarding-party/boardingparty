package board

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

var zeroTime time.Time

const testPort = 4321

var testHost = "127.0.0.1:4321"

// fakeForwarder hands out the URLs in bases, one per Open call; once they run
// out it repeats the last one.
type fakeForwarder struct {
	mu     sync.Mutex
	bases  []string
	opens  int
	closed int
}

func (f *fakeForwarder) Open(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.opens
	if i >= len(f.bases) {
		i = len(f.bases) - 1
	}
	f.opens++
	return f.bases[i], nil
}

func (f *fakeForwarder) Close() {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
}

func (f *fakeForwarder) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// deadURL returns a base URL nothing listens on: dialling it is refused.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

var testUI = fstest.MapFS{
	"index.html": {Data: []byte("<!doctype html><p>index</p>")},
	"app.js":     {Data: []byte("console.log('app')")},
}

func newTestHandler(t *testing.T, fw Forwarder) (http.Handler, *Proxy) {
	t.Helper()
	p := NewProxy(fw)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewHandler(Config{Token: "secret-token", Port: testPort, Proxy: p, UI: testUI}), p
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	if r.Host == "example.com" { // httptest default
		r.Host = testHost
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func authed(r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: CookieName(testPort), Value: "secret-token"})
	return r
}

func backend(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Cookie") != "" {
			http.Error(w, "cookie leaked to the director", http.StatusTeapot)
			return
		}
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "backend:"+r.Method+" "+r.URL.RequestURI())
	}))
	t.Cleanup(s.Close)
	return s
}

func TestUnauthenticatedIs401Everywhere(t *testing.T) {
	var hits atomic.Int32
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{backend(t, &hits).URL}})
	for _, path := range []string{"/", "/app.js", "/index.html", "/api/runs", "/api/project", "/no/such/page"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			w := do(h, httptest.NewRequest(m, path, nil))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401", m, path, w.Code)
			}
			if strings.Contains(w.Body.String(), "index") || strings.Contains(w.Body.String(), "backend") {
				t.Errorf("%s %s leaked content: %q", m, path, w.Body.String())
			}
		}
	}
	if hits.Load() != 0 {
		t.Errorf("director hit %d times without a token", hits.Load())
	}
}

func TestWrongCookieOrKeyIs401(t *testing.T) {
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName(testPort), Value: "secret-tokeX"})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie = %d", w.Code)
	}
	w := do(h, httptest.NewRequest(http.MethodGet, "/?key=nope", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong key = %d", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Errorf("wrong key set a cookie")
	}
	// A cookie for another board on another port doesn't count.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName(9999), Value: "secret-token"})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Errorf("other port's cookie = %d", w.Code)
	}
}

func TestKeySetsCookieAndRedirectsWithoutKey(t *testing.T) {
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	w := do(h, httptest.NewRequest(http.MethodGet, "/?key=secret-token", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies = %v", cs)
	}
	c := cs[0]
	if c.Name != CookieName(testPort) || c.Value != "secret-token" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie = %+v", c)
	}
	if strings.Contains(w.Body.String(), "secret-token") {
		t.Errorf("token in body")
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", w.Header().Get("Referrer-Policy"))
	}

	// Other query parameters and the path survive.
	w = do(h, httptest.NewRequest(http.MethodGet, "/runs?x=1&key=secret-token", nil))
	if loc := w.Header().Get("Location"); loc != "/runs?x=1" {
		t.Errorf("Location = %q", loc)
	}

	// The cookie then works.
	w = do(h, authed(httptest.NewRequest(http.MethodGet, "/", nil)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "index") {
		t.Errorf("with cookie = %d %q", w.Code, w.Body.String())
	}
}

func TestWrongHostIs403(t *testing.T) {
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	for _, host := range []string{"evil.example:4321", "evil.example", "127.0.0.1:9999", "127.0.0.1", "localhost", "[::1]:4321", "0.0.0.0:4321"} {
		r := authed(httptest.NewRequest(http.MethodGet, "/?key=secret-token", nil))
		r.Host = host
		if w := do(h, r); w.Code != http.StatusForbidden {
			t.Errorf("Host %q = %d, want 403", host, w.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:4321", "localhost:4321", "LOCALHOST:4321"} {
		r := authed(httptest.NewRequest(http.MethodGet, "/", nil))
		r.Host = host
		if w := do(h, r); w.Code != http.StatusOK {
			t.Errorf("Host %q = %d, want 200", host, w.Code)
		}
	}
}

func TestCrossSiteStateChangeIs403(t *testing.T) {
	var hits atomic.Int32
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{backend(t, &hits).URL}})
	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"other port origin", map[string]string{"Origin": "http://127.0.0.1:9999"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:4321"}, http.StatusCreated},
		{"localhost origin", map[string]string{"Origin": "http://localhost:4321"}, http.StatusCreated},
		{"no headers (curl)", nil, http.StatusCreated},
	}
	for _, c := range cases {
		r := authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}")))
		for k, v := range c.header {
			r.Header.Set(k, v)
		}
		if w := do(h, r); w.Code != c.want {
			t.Errorf("%s = %d, want %d", c.name, w.Code, c.want)
		}
	}
	if hits.Load() != 3 {
		t.Errorf("director hits = %d, want 3", hits.Load())
	}
	// A cross-site GET (e.g. a link) is fine; SameSite=Strict keeps the cookie off it anyway.
	r := authed(httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := do(h, r); w.Code != http.StatusCreated {
		t.Errorf("cross-site GET = %d", w.Code)
	}
}

func TestStaticFilesAndSPAFallback(t *testing.T) {
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	w := do(h, authed(httptest.NewRequest(http.MethodGet, "/app.js", nil)))
	if w.Code != http.StatusOK || w.Body.String() != "console.log('app')" {
		t.Errorf("app.js = %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	for _, p := range []string{"/runs/abc", "/project", "/index.html", "/../../etc/passwd"} {
		w := do(h, authed(httptest.NewRequest(http.MethodGet, p, nil)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "index") {
			t.Errorf("%s = %d %q", p, w.Code, w.Body.String())
		}
	}
	if w := do(h, authed(httptest.NewRequest(http.MethodPost, "/app.js", nil))); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /app.js = %d", w.Code)
	}
}

func TestEmbeddedUIHasIndex(t *testing.T) {
	ui, err := UI()
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Token: "t", Port: testPort, Proxy: NewProxy(&fakeForwarder{bases: []string{deadURL(t)}}), UI: ui})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName(testPort), Value: "t"})
	if w := do(h, r); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "boardingparty") {
		t.Errorf("embedded index = %d %q", w.Code, w.Body.String())
	}
}

func TestProxyPassesThroughStatusAndBody(t *testing.T) {
	var hits atomic.Int32
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{backend(t, &hits).URL}})
	w := do(h, authed(httptest.NewRequest(http.MethodGet, "/api/runs?limit=5", nil)))
	if w.Code != http.StatusCreated {
		t.Errorf("code = %d (%s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "backend:GET /api/runs?limit=5" {
		t.Errorf("body = %q", got)
	}
	if w.Header().Get("X-Path") != "/api/runs" {
		t.Errorf("headers not passed through")
	}
}

func TestProxyStreamsRangeRequests(t *testing.T) {
	data := strings.Repeat("0123456789", 1000)
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.webm", zeroTime, strings.NewReader(data))
	}))
	defer dir.Close()
	p := NewProxy(&fakeForwarder{bases: []string{dir.URL}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := authed(httptest.NewRequest(http.MethodGet, "/api/runs/r1/artifacts/video.webm", nil))
	r.Header.Set("Range", "bytes=10-19")
	w := do(NewHandler(Config{Token: "secret-token", Port: testPort, Proxy: p, UI: testUI}), r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "0123456789" {
		t.Errorf("range = %d %q", w.Code, w.Body.String())
	}
}

func TestProxyReconnectsOnDialErrorThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	fw := &fakeForwarder{bases: []string{deadURL(t), backend(t, &hits).URL}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if w.Code != http.StatusCreated || w.Body.String() != "backend:GET /api/runs" {
		t.Errorf("after reconnect = %d %q", w.Code, w.Body.String())
	}
	if fw.openCount() != 2 {
		t.Errorf("opens = %d, want 2", fw.openCount())
	}
	if fw.closed != 1 {
		t.Errorf("old forward closed %d times, want 1", fw.closed)
	}
	// The next request uses the new forward directly.
	do(h, authed(httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if fw.openCount() != 2 || hits.Load() != 2 {
		t.Errorf("opens = %d hits = %d", fw.openCount(), hits.Load())
	}
}

func TestProxyReconnectsOnlyOnce(t *testing.T) {
	fw := &fakeForwarder{bases: []string{deadURL(t), deadURL(t)}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "director unavailable") {
		t.Errorf("still down = %d %q", w.Code, w.Body.String())
	}
	if fw.openCount() != 2 { // the start plus one reopen, not a loop
		t.Errorf("opens = %d, want 2", fw.openCount())
	}
}

func TestProxyPostDuringOutageIs502WithoutRetry(t *testing.T) {
	var hits atomic.Int32
	fw := &fakeForwarder{bases: []string{deadURL(t), backend(t, &hits).URL}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"persona":"a"}`))))
	if w.Code != http.StatusBadGateway {
		t.Errorf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "director unavailable — retrying connection") {
		t.Errorf("body = %q", w.Body.String())
	}
	if hits.Load() != 0 {
		t.Errorf("POST was retried (%d hits)", hits.Load())
	}
	if fw.openCount() != 2 {
		t.Errorf("opens = %d, want 2 (reconnected for the next request)", fw.openCount())
	}
	// The user's retry goes through on the new forward.
	w = do(h, authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{}`))))
	if w.Code != http.StatusCreated || hits.Load() != 1 {
		t.Errorf("retry = %d hits %d", w.Code, hits.Load())
	}
}

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if len(a) != 64 || a == b || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("tokens %q %q", a, b)
	}
}
