package board

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"syscall"
)

// unavailableMsg is what the browser sees when the director can't be reached.
const unavailableMsg = "director unavailable — retrying connection"

// Forwarder opens a connection to the director admin API, typically a kube
// port-forward. Open replaces any previous connection and returns its base
// URL; Close tears the current one down.
type Forwarder interface {
	Open(ctx context.Context) (string, error)
	Close()
}

// Proxy reverse-proxies /api/* to the director through a Forwarder. When the
// forward is gone (the director pod restarted, say) it re-opens it once and,
// for safe requests, retries; anything else gets a 502 so the user retries.
type Proxy struct {
	fw Forwarder
	rp *httputil.ReverseProxy

	mu   sync.Mutex
	base *url.URL // current forward; nil before Start
}

// NewProxy builds a proxy over fw. Call Start before serving.
func NewProxy(fw Forwarder) *Proxy {
	p := &Proxy{fw: fw}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the forward is local; never send it through HTTP_PROXY
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.current())
			// The launch token is for this server only; the director never sees it.
			pr.Out.Header.Del("Cookie")
		},
		Transport:    &reconnectTransport{p: p, next: tr},
		ErrorLog:     log.New(io.Discard, "", 0),
		ErrorHandler: p.errorHandler,
	}
	return p
}

// Start opens the first forward.
func (p *Proxy) Start(ctx context.Context) error {
	b, err := p.fw.Open(ctx)
	if err != nil {
		return err
	}
	u, err := url.Parse(b)
	if err != nil {
		p.fw.Close()
		return err
	}
	p.mu.Lock()
	p.base = u
	p.mu.Unlock()
	return nil
}

// Close tears down the forward.
func (p *Proxy) Close() { p.fw.Close() }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.rp.ServeHTTP(w, r) }

func (p *Proxy) current() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base == nil {
		return &url.URL{Scheme: "http", Host: "127.0.0.1:0"}
	}
	return p.base
}

// reopen replaces the forward that failed (failedHost) and returns the new
// one. If another request already replaced it, that one is used as is, so a
// burst of failures reconnects once.
func (p *Proxy) reopen(ctx context.Context, failedHost string) (*url.URL, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base != nil && p.base.Host != failedHost {
		return p.base, nil
	}
	p.fw.Close()
	b, err := p.fw.Open(ctx)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(b)
	if err != nil {
		return nil, err
	}
	p.base = u
	return u, nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the browser went away (e.g. seeking the video); nothing to tell it
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": unavailableMsg})
}

// reconnectTransport re-opens the forward on a connection error and retries
// GET/HEAD once on the new one.
type reconnectTransport struct {
	p    *Proxy
	next http.RoundTripper
}

func (t *reconnectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil || !connLost(err) || req.Context().Err() != nil {
		return resp, err
	}
	u, rerr := t.p.reopen(req.Context(), req.URL.Host)
	if rerr != nil {
		return nil, errors.Join(err, rerr)
	}
	if !retryable(req) {
		return nil, err
	}
	r2 := req.Clone(req.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = u.Scheme, u.Host, u.Host
	return t.next.RoundTrip(r2)
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
// refuses connections, or the connection broke before a response came back.
func connLost(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
