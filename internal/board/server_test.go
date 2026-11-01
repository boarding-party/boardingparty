package board

import (
	"context"
	"errors"
	"io"
	"io/fs"
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

const (
	testPort  = 4321
	testHost  = "127.0.0.1:4321"
	testAdmin = "admin-secret"
)

// fakeForwarder hands out the URLs in bases, one per Open call; once they run
// out it repeats the last one.
type fakeForwarder struct {
	mu     sync.Mutex
	bases  []string
	opens  int
	closed int
	delay  time.Duration
}

func (f *fakeForwarder) Open(context.Context) (string, error) {
	time.Sleep(f.delay)
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

func (f *fakeForwarder) counts() (opens, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens, f.closed
}

func (f *fakeForwarder) openCount() int { o, _ := f.counts(); return o }

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

// hangUpURL accepts connections and closes them at once, like a port-forward
// whose pod has gone.
func hangUpURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return "http://" + l.Addr().String()
}

var testUI = fstest.MapFS{
	"index.html": {Data: []byte("<!doctype html><p>index</p>")},
	"app.js":     {Data: []byte("console.log('app')")},
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newHandlerWith(t *testing.T, p http.Handler, ui fs.FS, now func() time.Time) (*Handler, string) {
	t.Helper()
	h, err := NewHandler(Config{Port: testPort, Proxy: p, UI: ui, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	key, err := h.NewLink()
	if err != nil {
		t.Fatal(err)
	}
	return h, key
}

// newTestHandler returns a started handler and its launch key.
func newTestHandler(t *testing.T, fw Forwarder) (*Handler, string) {
	t.Helper()
	p := NewProxy(fw, testAdmin)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return newHandlerWith(t, p, testUI, nil)
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	if r.Host == "example.com" { // httptest default
		r.Host = testHost
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func authed(h *Handler, r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: CookieName(testPort), Value: h.session})
	return r
}

// backend is a fake director: it echoes the request line and records what
// headers reached it.
type fakeDirector struct {
	hits atomic.Int32
	mu   sync.Mutex
	last http.Header
	host string
}

func (d *fakeDirector) lastReq() (http.Header, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last, d.host
}

func backendDir(t *testing.T) (*httptest.Server, *fakeDirector) {
	t.Helper()
	d := &fakeDirector{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.hits.Add(1)
		d.mu.Lock()
		d.last, d.host = r.Header.Clone(), r.Host
		d.mu.Unlock()
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "backend:"+r.Method+" "+r.URL.RequestURI())
	}))
	t.Cleanup(s.Close)
	return s, d
}

func backend(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
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
		t.Errorf("director hit %d times without a session", hits.Load())
	}
}

func TestWrongCookieOrKeyIs401(t *testing.T) {
	h, key := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName(testPort), Value: h.session + "x"})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie = %d", w.Code)
	}
	// The launch key is not a session: as a cookie it gets nothing.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName(testPort), Value: key})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Errorf("key as cookie = %d", w.Code)
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
	r.AddCookie(&http.Cookie{Name: CookieName(9999), Value: h.session})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Errorf("other port's cookie = %d", w.Code)
	}
	// A wrong guess doesn't burn the real key.
	if w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+key, nil)); w.Code != http.StatusSeeOther {
		t.Errorf("real key after a wrong guess = %d", w.Code)
	}
}

func TestKeySetsSessionCookieAndRedirectsWithoutKey(t *testing.T) {
	h, key := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+key, nil))
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
	if c.Name != CookieName(testPort) || c.Value != h.session || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie = %+v", c)
	}
	if c.Value == key || len(c.Value) != 64 {
		t.Errorf("session cookie must be a separate random value: %q vs key %q", c.Value, key)
	}
	if strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), h.session) {
		t.Errorf("secret in body")
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", w.Header().Get("Referrer-Policy"))
	}

	// The cookie then works.
	w = do(h, authed(h, httptest.NewRequest(http.MethodGet, "/", nil)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "index") {
		t.Errorf("with cookie = %d %q", w.Code, w.Body.String())
	}
	// Reloading the link with the session just redirects; path and other params survive.
	w = do(h, authed(h, httptest.NewRequest(http.MethodGet, "/runs?x=1&key="+key, nil)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/runs?x=1" {
		t.Errorf("reload = %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestKeyIsSingleUse(t *testing.T) {
	h, key := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	if w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+key, nil)); w.Code != http.StatusSeeOther {
		t.Fatalf("first use = %d", w.Code)
	}
	w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+key, nil))
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Errorf("second use = %d cookies %v", w.Code, w.Result().Cookies())
	}
	if !strings.Contains(w.Body.String(), "press Enter") {
		t.Errorf("no hint: %q", w.Body.String())
	}
	// A new link works once, and replaces any unused one.
	k2, _ := h.NewLink()
	k3, _ := h.NewLink()
	if k2 == key || k3 == k2 {
		t.Fatal("links repeat")
	}
	if w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+k2, nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("superseded link = %d", w.Code)
	}
	if w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+k3, nil)); w.Code != http.StatusSeeOther {
		t.Errorf("new link = %d", w.Code)
	}
}

func TestKeyExpires(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	h, key := newHandlerWith(t, http.NotFoundHandler(), testUI, c.now)
	c.add(DefaultLinkTTL - time.Second)
	k2, _ := h.NewLink() // fresh 10 minutes from now
	c.add(2 * time.Second)
	_ = key
	if w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+k2, nil)); w.Code != http.StatusSeeOther {
		t.Errorf("within TTL = %d", w.Code)
	}
	k3, _ := h.NewLink()
	c.add(DefaultLinkTTL)
	if w := do(h, httptest.NewRequest(http.MethodGet, "/?key="+k3, nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("expired = %d", w.Code)
	}
}

func TestWrongHostIs403(t *testing.T) {
	h, key := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	for _, host := range []string{"evil.example:4321", "evil.example", "127.0.0.1:9999", "127.0.0.1", "localhost", "[::1]:4321", "0.0.0.0:4321"} {
		r := authed(h, httptest.NewRequest(http.MethodGet, "/?key="+key, nil))
		r.Host = host
		if w := do(h, r); w.Code != http.StatusForbidden {
			t.Errorf("Host %q = %d, want 403", host, w.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:4321", "localhost:4321", "LOCALHOST:4321"} {
		r := authed(h, httptest.NewRequest(http.MethodGet, "/", nil))
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
		r := authed(h, httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}")))
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
	r := authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := do(h, r); w.Code != http.StatusCreated {
		t.Errorf("cross-site GET = %d", w.Code)
	}
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	srv, _ := backendDir(t)
	h, key := newTestHandler(t, &fakeForwarder{bases: []string{srv.URL}})
	reqs := map[string]*http.Request{
		"401":    httptest.NewRequest(http.MethodGet, "/", nil),
		"login":  httptest.NewRequest(http.MethodGet, "/?key="+key, nil),
		"static": authed(h, httptest.NewRequest(http.MethodGet, "/app.js", nil)),
		"api":    authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil)),
	}
	for name, r := range reqs {
		w := do(h, r)
		hd := w.Header()
		if hd.Get("Content-Security-Policy") != CSP || hd.Get("X-Frame-Options") != "DENY" || hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", name, hd)
		}
		if len(hd.Values("X-Content-Type-Options")) != 1 || len(hd.Values("Content-Security-Policy")) != 1 {
			t.Errorf("%s: doubled headers %v", name, hd)
		}
		if name == "api" && hd.Get("Cache-Control") != "no-store" {
			t.Errorf("api Cache-Control = %q", hd.Get("Cache-Control"))
		}
	}
	if !strings.Contains(CSP, "frame-ancestors 'none'") || !strings.Contains(CSP, "media-src 'self' blob:") {
		t.Errorf("CSP = %q", CSP)
	}
}

func TestAPIKeepsDirectorCacheControl(t *testing.T) {
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write([]byte("v"))
	}))
	defer dir.Close()
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{dir.URL}})
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs/r1/artifacts/a.webm", nil)))
	if got := w.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "max-age=60" {
		t.Errorf("Cache-Control = %v", got)
	}
	if got := w.Header().Values("X-Content-Type-Options"); len(got) != 1 {
		t.Errorf("nosniff = %v", got)
	}
}

func TestStaticFilesAndSPAFallback(t *testing.T) {
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{deadURL(t)}})
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/app.js", nil)))
	if w.Code != http.StatusOK || w.Body.String() != "console.log('app')" {
		t.Errorf("app.js = %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	for _, p := range []string{"/runs/abc", "/project", "/index.html", "/../../etc/passwd"} {
		w := do(h, authed(h, httptest.NewRequest(http.MethodGet, p, nil)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "index") {
			t.Errorf("%s = %d %q", p, w.Code, w.Body.String())
		}
	}
	if w := do(h, authed(h, httptest.NewRequest(http.MethodPost, "/app.js", nil))); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /app.js = %d", w.Code)
	}
}

func TestEmbeddedUIHasIndex(t *testing.T) {
	ui, err := UI()
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Config{Port: testPort, Proxy: http.NotFoundHandler(), UI: ui})
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/", nil)))
	if w.Code != http.StatusOK || !strings.Contains(strings.ToLower(w.Body.String()), "<html") {
		t.Errorf("embedded index = %d %q", w.Code, w.Body.String())
	}
}

func TestProxyPassesThroughStatusAndBody(t *testing.T) {
	var hits atomic.Int32
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{backend(t, &hits).URL}})
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs?limit=5", nil)))
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

func TestProxyInjectsBearerAndDropsBrowserHeaders(t *testing.T) {
	srv, dir := backendDir(t)
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{srv.URL}})
	r := authed(h, httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}")))
	r.Header.Set("Authorization", "Bearer from-the-browser")
	r.Header.Set("Origin", "http://127.0.0.1:4321")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	r.Header.Set("Referer", "http://127.0.0.1:4321/")
	r.Header.Set("Content-Type", "application/json")
	if w := do(h, r); w.Code != http.StatusCreated {
		t.Fatalf("code = %d %s", w.Code, w.Body.String())
	}
	got, host := dir.lastReq()
	if v := got.Values("Authorization"); len(v) != 1 || v[0] != "Bearer "+testAdmin {
		t.Errorf("Authorization = %v", v)
	}
	for _, k := range []string{"Cookie", "Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Referer"} {
		if got.Get(k) != "" {
			t.Errorf("%s forwarded: %q", k, got.Get(k))
		}
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type lost")
	}
	if want := strings.TrimPrefix(srv.URL, "http://"); host != want || !strings.HasPrefix(host, "127.0.0.1:") {
		t.Errorf("outbound Host = %q, want %q", host, want)
	}
}

func TestProxyStreamsRangeRequests(t *testing.T) {
	data := strings.Repeat("0123456789", 1000)
	dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.webm", zeroTime, strings.NewReader(data))
	}))
	defer dir.Close()
	h, _ := newTestHandler(t, &fakeForwarder{bases: []string{dir.URL}})
	r := authed(h, httptest.NewRequest(http.MethodGet, "/api/runs/r1/artifacts/video.webm", nil))
	r.Header.Set("Range", "bytes=10-19")
	w := do(h, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "0123456789" {
		t.Errorf("range = %d %q", w.Code, w.Body.String())
	}
}

func TestProxyReconnectsOnDialErrorThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	fw := &fakeForwarder{bases: []string{deadURL(t), backend(t, &hits).URL}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if w.Code != http.StatusCreated || w.Body.String() != "backend:GET /api/runs" {
		t.Errorf("after reconnect = %d %q", w.Code, w.Body.String())
	}
	if o, c := fw.counts(); o != 2 || c != 1 {
		t.Errorf("opens = %d closes = %d, want 2 and 1", o, c)
	}
	// The next request uses the new forward directly.
	do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if fw.openCount() != 2 || hits.Load() != 2 {
		t.Errorf("opens = %d hits = %d", fw.openCount(), hits.Load())
	}
}

func TestProxyReconnectsWhenForwardHangsUp(t *testing.T) {
	var hits atomic.Int32
	fw := &fakeForwarder{bases: []string{hangUpURL(t), backend(t, &hits).URL}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if w.Code != http.StatusCreated || fw.openCount() != 2 {
		t.Errorf("after hang-up = %d, opens %d", w.Code, fw.openCount())
	}
}

func TestProxyReconnectsOnlyOnce(t *testing.T) {
	fw := &fakeForwarder{bases: []string{deadURL(t), deadURL(t)}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "director unavailable") {
		t.Errorf("still down = %d %q", w.Code, w.Body.String())
	}
	if fw.openCount() != 2 { // the start plus one reopen, not a loop
		t.Errorf("opens = %d, want 2", fw.openCount())
	}
}

func TestProxyConcurrentFailuresReconnectOnce(t *testing.T) {
	var hits atomic.Int32
	fw := &fakeForwarder{bases: []string{deadURL(t), backend(t, &hits).URL}, delay: 20 * time.Millisecond}
	h, _ := newTestHandler(t, fw)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil))).Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Errorf("request %d = %d", i, c)
		}
	}
	if fw.openCount() != 2 {
		t.Errorf("opens = %d, want 2", fw.openCount())
	}
}

func TestProxyPostDuringOutageIs502WithoutRetry(t *testing.T) {
	var hits atomic.Int32
	fw := &fakeForwarder{bases: []string{deadURL(t), backend(t, &hits).URL}}
	h, _ := newTestHandler(t, fw)
	w := do(h, authed(h, httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"persona":"a"}`))))
	if w.Code != http.StatusBadGateway {
		t.Errorf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "director unavailable — retrying connection") {
		t.Errorf("body = %q", w.Body.String())
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("502 without security headers")
	}
	if hits.Load() != 0 {
		t.Errorf("POST was retried (%d hits)", hits.Load())
	}
	if fw.openCount() != 2 {
		t.Errorf("opens = %d, want 2 (reconnected for the next request)", fw.openCount())
	}
	// The user's retry goes through on the new forward.
	w = do(h, authed(h, httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{}`))))
	if w.Code != http.StatusCreated || hits.Load() != 1 {
		t.Errorf("retry = %d hits %d", w.Code, hits.Load())
	}
}

func TestProxyNoReconnectAfterClose(t *testing.T) {
	fw := &fakeForwarder{bases: []string{deadURL(t), deadURL(t)}}
	p := NewProxy(fw, testAdmin)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Close()
	h, _ := newHandlerWith(t, p, testUI, nil)
	w := do(h, authed(h, httptest.NewRequest(http.MethodGet, "/api/runs", nil)))
	if w.Code != http.StatusBadGateway {
		t.Errorf("code = %d", w.Code)
	}
	if fw.openCount() != 1 {
		t.Errorf("reopened after Close: opens = %d", fw.openCount())
	}
	if _, _, err := p.reopen(context.Background(), 1); err != errClosed {
		t.Errorf("reopen after close = %v", err)
	}
}

func TestProxyCloseDuringReopenDiscardsNewForward(t *testing.T) {
	fw := &fakeForwarder{bases: []string{deadURL(t), deadURL(t)}, delay: 0}
	p := NewProxy(fw, testAdmin)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fw.mu.Lock()
	fw.delay = 50 * time.Millisecond
	fw.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, _, err := p.reopen(context.Background(), 1); done <- err }()
	time.Sleep(10 * time.Millisecond)
	p.Close()
	if err := <-done; err != errClosed {
		t.Errorf("reopen = %v", err)
	}
	// Close from p.Close, the pre-open Close in reopen, and the discard.
	if _, c := fw.counts(); c != 3 {
		t.Errorf("closes = %d, want 3", c)
	}
}

func TestConnLostOnlyForFreshConnections(t *testing.T) {
	dial := &net.OpError{Op: "dial", Err: io.EOF}
	if !connLost(dial, false) {
		t.Error("dial error not lost")
	}
	if connLost(io.EOF, false) {
		t.Error("EOF on a reused keep-alive connection treated as forward gone")
	}
	if !connLost(io.EOF, true) {
		t.Error("EOF on a fresh connection not lost")
	}
	if connLost(errors.New("http: server closed idle connection"), false) {
		t.Error("idle close on a reused connection treated as forward gone")
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
