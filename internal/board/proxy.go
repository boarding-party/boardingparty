package board

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// unavailableMsg is what the browser sees when the director can't be reached.
const unavailableMsg = "director unavailable — retrying connection"

// openTimeout bounds one re-open of the forward. It's detached from the
// request that noticed the failure, so a cancelled request doesn't abort a
// reconnect other requests are waiting on.
var openTimeout = 30 * time.Second

var errClosed = errors.New("board is shutting down")

// Forwarder opens a connection to the director admin API, typically a kube
// port-forward. Open replaces any previous connection and returns its base
// URL; Close tears the current one down.
type Forwarder interface {
	Open(ctx context.Context) (string, error)
	Close()
}

// Proxy reverse-proxies /api/* to the director through a Forwarder, adding the
// admin bearer secret. When the forward is gone (the director pod restarted,
// say) it re-opens it once and, for GET/HEAD, retries; anything else gets a
// 502 so the user retries.
type Proxy struct {
	fw    Forwarder
	token string
	rp    *httputil.ReverseProxy

	mu      sync.Mutex
	base    *url.URL      // current forward; nil before Start
	gen     uint64        // bumped on every successful (re)open
	opening chan struct{} // non-nil while a reopen is in flight
	closed  bool
}

// NewProxy builds a proxy over fw that authenticates to the director with
// adminToken. Call Start before serving.
func NewProxy(fw Forwarder, adminToken string) *Proxy {
	p := &Proxy{fw: fw, token: adminToken}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the forward is local; never send it through HTTP_PROXY
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Scheme and path only; the transport pins the host per attempt.
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1"})
			h := pr.Out.Header
			// Browser-supplied credentials and cross-origin markers never reach
			// the director: the session cookie is this server's, and the
			// director refuses anything that looks like a browser.
			for _, k := range []string{"Cookie", "Authorization", "Origin", "Referer",
				"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User"} {
				h.Del(k)
			}
			h.Set("Authorization", "Bearer "+p.token)
		},
		Transport:      &reconnectTransport{p: p, next: tr},
		ModifyResponse: func(r *http.Response) error { apiHeaders(r.Header); return nil },
		ErrorLog:       log.New(io.Discard, "", 0),
		ErrorHandler:   p.errorHandler,
	}
	return p
}

// apiHeaders: our security headers win; caching is the director's call, and
// no-store when it doesn't say.
func apiHeaders(h http.Header) {
	cc := h.Get("Cache-Control")
	SecurityHeaders(h)
	if cc != "" {
		h.Set("Cache-Control", cc)
	}
}

// Start opens the first forward.
func (p *Proxy) Start(ctx context.Context) error {
	b, err := p.fw.Open(ctx)
	if err != nil {
		return err
	}
	u, err := parseBase(b)
	if err != nil {
		p.fw.Close()
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.fw.Close()
		return errClosed
	}
	p.base, p.gen = u, p.gen+1
	return nil
}

// Close tears down the forward; later reconnects are refused.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.fw.Close()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.rp.ServeHTTP(w, r) }

func parseBase(b string) (*url.URL, error) {
	u, err := url.Parse(b)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("forward returned no host")
	}
	return u, nil
}

func (p *Proxy) current() (*url.URL, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, 0, errClosed
	}
	if p.base == nil {
		return nil, 0, errors.New("proxy not started")
	}
	return p.base, p.gen, nil
}

// reopen replaces the forward of generation failed and returns the new one.
// If it was already replaced, the current one is returned; if a reopen is in
// flight, it waits for that one. The Open itself runs outside the lock.
func (p *Proxy) reopen(ctx context.Context, failed uint64) (*url.URL, uint64, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, 0, errClosed
		}
		if p.gen != failed && p.base != nil {
			u, g := p.base, p.gen
			p.mu.Unlock()
			return u, g, nil
		}
		if ch := p.opening; ch != nil {
			p.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			p.mu.Lock()
			moved := p.gen != failed
			p.mu.Unlock()
			if !moved {
				return nil, 0, errors.New("reconnect failed")
			}
			continue
		}
		ch := make(chan struct{})
		p.opening = ch
		p.mu.Unlock()

		p.fw.Close()
		octx, cancel := context.WithTimeout(context.Background(), openTimeout)
		b, err := p.fw.Open(octx)
		cancel()
		var u *url.URL
		if err == nil {
			u, err = parseBase(b)
		}

		p.mu.Lock()
		p.opening = nil
		close(ch)
		switch {
		case p.closed:
			// Shut down while we were opening: don't leak the new forward.
			p.mu.Unlock()
			if err == nil {
				p.fw.Close()
			}
			return nil, 0, errClosed
		case err != nil:
			p.mu.Unlock()
			return nil, 0, err
		case p.gen != failed:
			// Someone else moved it on (Start); keep theirs.
			cu, cg := p.base, p.gen
			p.mu.Unlock()
			return cu, cg, nil
		}
		p.base, p.gen = u, p.gen+1
		g := p.gen
		p.mu.Unlock()
		return u, g, nil
	}
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the browser went away (e.g. seeking the video); nothing to tell it
	}
	SecurityHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": unavailableMsg})
}

// reconnectTransport sends each attempt to the current forward, re-opens it
// when the connection is gone, and retries GET/HEAD once on the new one.
type reconnectTransport struct {
	p    *Proxy
	next http.RoundTripper
}

func (t *reconnectTransport) attempt(req *http.Request, u *url.URL) (*http.Response, bool, error) {
	fresh := false
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { fresh = !i.Reused }}
	r := req.Clone(httptrace.WithClientTrace(req.Context(), trace))
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	r.Host = u.Host // the director checks for a loopback Host
	resp, err := t.next.RoundTrip(r)
	return resp, err != nil && connLost(err, fresh), err
}

func (t *reconnectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, gen, err := t.p.current()
	if err != nil {
		return nil, err
	}
	resp, lost, err := t.attempt(req, u)
	if !lost || req.Context().Err() != nil {
		return resp, err
	}
	u, _, rerr := t.p.reopen(req.Context(), gen)
	if rerr != nil {
		return nil, errors.Join(err, rerr)
	}
	if !retryable(req) {
		return nil, err
	}
	resp, _, err = t.attempt(req, u)
	return resp, err
}

// retryable: only safe requests with no body. A POST may have reached the
// director before the connection broke, so it's never sent twice.
func retryable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return r.Body == nil || r.Body == http.NoBody
}

// connLost reports whether err means the forward is gone: the local port
// refuses connections, or a connection we just dialled broke before a
// response (a port-forward whose pod is gone accepts, then hangs up). An
// error on a reused keep-alive connection is just that connection.
func connLost(err error, freshConn bool) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return freshConn && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isReset(err) ||
		// net/http's unexported errServerClosedIdle: the peer closed a
		// connection we had only just opened.
		strings.Contains(err.Error(), "server closed idle connection"))
}

func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "read"
}
