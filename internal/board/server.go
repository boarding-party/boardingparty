// Package board serves the boardingparty UI on localhost and proxies its API
// calls to the director.
//
// The server is an unauthenticated admin API's front door, so every request
// is checked: the Host header must name this server (DNS-rebinding guard),
// the per-launch token must arrive as ?key= (first load) or as the cookie it
// sets, and state-changing requests must not come from another site.
package board

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed ui
var embedded embed.FS

// UI returns the embedded single-page app.
func UI() (fs.FS, error) { return fs.Sub(embedded, "ui") }

// NewToken returns 32 random bytes, hex-encoded.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CookieName is per port: cookies aren't port-scoped, so two boards open at
// once would otherwise overwrite each other's token.
func CookieName(port int) string { return "bp_board_" + strconv.Itoa(port) }

// Config is what the handler needs.
type Config struct {
	Token string // launch token
	Port  int    // the port the server listens on, for the Host and Origin checks
	Proxy http.Handler
	UI    fs.FS
}

type handler struct {
	cfg     Config
	hosts   map[string]bool
	origins map[string]bool
	cookie  string
}

// NewHandler returns the guarded handler: /api/* goes to cfg.Proxy, the rest
// is the UI with index.html as the fallback for client-side routes.
func NewHandler(cfg Config) http.Handler {
	p := strconv.Itoa(cfg.Port)
	h := &handler{
		cfg:     cfg,
		hosts:   map[string]bool{"127.0.0.1:" + p: true, "localhost:" + p: true},
		origins: map[string]bool{"http://127.0.0.1:" + p: true, "http://localhost:" + p: true},
		cookie:  CookieName(cfg.Port),
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Cache-Control", "no-store")

	if !h.hosts[strings.ToLower(r.Host)] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Query().Has("key") {
		h.login(w, r)
		return
	}
	if c, err := r.Cookie(h.cookie); err != nil || !h.tokenOK(c.Value) {
		http.Error(w, "unauthorised: open the link boardingparty board printed", http.StatusUnauthorized)
		return
	}
	if !safeMethod(r.Method) && h.crossSite(r) {
		http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		hd.Del("Cache-Control") // the director decides
		h.cfg.Proxy.ServeHTTP(w, r)
		return
	}
	h.static(w, r)
}

func (h *handler) tokenOK(v string) bool {
	return subtle.ConstantTimeCompare([]byte(v), []byte(h.cfg.Token)) == 1
}

// login trades ?key= for the cookie and redirects to the same URL without the
// key, so the token doesn't stay in the address bar or history.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !h.tokenOK(q.Get("key")) {
		http.Error(w, "unauthorised: wrong key", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: h.cookie, Value: h.cfg.Token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	q.Del("key")
	loc := r.URL.EscapedPath()
	if loc == "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		loc = "/" // never an open redirect
	}
	if enc := q.Encode(); enc != "" {
		loc += "?" + enc
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusSeeOther)
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossSite reports whether the browser says the request came from another
// origin. Requests without either header (curl, old browsers) pass: they
// still need the cookie, which SameSite=Strict keeps off cross-site requests.
func (h *handler) crossSite(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" && !h.origins[strings.ToLower(o)] {
		return true
	}
	return false
}

// static serves a UI file, or index.html for any path that isn't one.
func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	if !safeMethod(r.Method) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || !h.serveFile(w, r, name) {
		h.serveFile(w, r, "index.html")
	}
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := h.cfg.UI.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, rs)
	return true
}
