// Package board serves the boardingparty UI on localhost and proxies its API
// calls to the director.
//
// The server fronts the director's admin API, so every request is checked:
// the Host header must name this server (DNS-rebinding guard), the browser
// must hold the session cookie, which it gets once by opening a single-use
// launch link (?key=), and state-changing requests must not come from
// another site.
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
	"sync"
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
// once would otherwise overwrite each other's session.
func CookieName(port int) string { return "bp_board_" + strconv.Itoa(port) }

// DefaultLinkTTL is how long a launch link stays valid if nobody opens it.
const DefaultLinkTTL = 10 * time.Minute

// CSP allows only this origin, plus the UI's web fonts.
const CSP = "default-src 'self'; img-src 'self' data:; media-src 'self' blob:; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; " +
	"frame-ancestors 'none'"

// SecurityHeaders sets the headers every response carries.
func SecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
}

// Config is what the handler needs.
type Config struct {
	Port    int // the port the server listens on, for the Host and Origin checks
	Proxy   http.Handler
	UI      fs.FS
	LinkTTL time.Duration    // default DefaultLinkTTL
	Now     func() time.Time // default time.Now
}

// Handler is the guarded board server.
type Handler struct {
	cfg     Config
	hosts   map[string]bool
	origins map[string]bool
	cookie  string
	session string // the cookie value; never printed, never in a URL

	mu      sync.Mutex
	nonce   string // current launch key; "" once used
	expires time.Time
}

// NewHandler returns the guarded handler: /api/* goes to cfg.Proxy, the rest
// is the UI with index.html as the fallback for client-side routes. Call
// NewLink for the key to put in the printed URL.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.LinkTTL <= 0 {
		cfg.LinkTTL = DefaultLinkTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	session, err := NewToken()
	if err != nil {
		return nil, err
	}
	p := strconv.Itoa(cfg.Port)
	return &Handler{
		cfg:     cfg,
		hosts:   map[string]bool{"127.0.0.1:" + p: true, "localhost:" + p: true},
		origins: map[string]bool{"http://127.0.0.1:" + p: true, "http://localhost:" + p: true},
		cookie:  CookieName(cfg.Port),
		session: session,
	}, nil
}

// NewLink issues a fresh single-use launch key, valid for LinkTTL, and
// invalidates any earlier one that hasn't been used.
func (h *Handler) NewLink() (string, error) {
	n, err := NewToken()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	h.nonce, h.expires = n, h.cfg.Now().Add(h.cfg.LinkTTL)
	h.mu.Unlock()
	return n, nil
}

// redeem consumes the launch key if it matches and hasn't expired.
func (h *Handler) redeem(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nonce == "" || !h.cfg.Now().Before(h.expires) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(h.nonce)) != 1 {
		return false
	}
	h.nonce = ""
	return true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	SecurityHeaders(w.Header())
	if !h.hosts[strings.ToLower(r.Host)] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	signedIn := h.hasSession(r)
	if r.URL.Query().Has("key") {
		h.login(w, r, signedIn)
		return
	}
	if !signedIn {
		http.Error(w, "unauthorised: open the link boardingparty board printed", http.StatusUnauthorized)
		return
	}
	if !safeMethod(r.Method) && h.crossSite(r) {
		http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		// The proxy sets these on the director's response; clear ours so they
		// aren't doubled.
		for k := range w.Header() {
			w.Header().Del(k)
		}
		h.cfg.Proxy.ServeHTTP(w, r)
		return
	}
	h.static(w, r)
}

func (h *Handler) hasSession(r *http.Request) bool {
	c, err := r.Cookie(h.cookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(h.session)) == 1
}

// login trades the launch key for the session cookie and redirects to the
// same URL without the key, so it doesn't stay in the address bar. A browser
// that already has the session (a reload of the link) is just redirected.
func (h *Handler) login(w http.ResponseWriter, r *http.Request, signedIn bool) {
	q := r.URL.Query()
	if !signedIn {
		if !h.redeem(q.Get("key")) {
			http.Error(w, "unauthorised: this link was already used or has expired; press Enter in the terminal running boardingparty board for a new one", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: h.cookie, Value: h.session, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
	}
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
func (h *Handler) crossSite(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" && !h.origins[strings.ToLower(o)] {
		return true
	}
	return false
}

// static serves a UI file, or index.html for any path that isn't one.
func (h *Handler) static(w http.ResponseWriter, r *http.Request) {
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

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
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
