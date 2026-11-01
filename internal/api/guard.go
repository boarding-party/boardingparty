package api

import (
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"strings"
)

// guardAdmin wraps the admin handler. The admin listener is reached through
// a kube port-forward, which is an ordinary TCP port on the operator's
// machine, so the API can't trust "it's loopback": every request needs the
// bearer secret, a loopback Host (DNS-rebinding guard) and no browser
// cross-origin markers. An empty token denies everything.
func guardAdmin(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// The CLI and board's proxy never send these; a browser always does
		// on a cross-origin request.
		if r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden: browser origin", http.StatusForbidden)
			return
		}
		if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "none" {
			http.Error(w, "forbidden: browser request", http.StatusForbidden)
			return
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	h = strings.ToLower(h)
	return h == "127.0.0.1" || h == "localhost"
}

// yamlTypes are accepted on POST /api/project.
var yamlTypes = map[string]bool{"application/yaml": true, "application/x-yaml": true, "text/yaml": true}

// mediaTypeIs reports whether the request's Content-Type is one of allowed.
// Missing, text/plain and form types are refused: those are what a browser
// can send cross-site without a preflight.
func mediaTypeIs(r *http.Request, allowed map[string]bool) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && allowed[mt]
}

var jsonType = map[string]bool{"application/json": true}
