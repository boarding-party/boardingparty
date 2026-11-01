package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errLostDirector is what a dropped port-forward looks like to the caller.
var errLostDirector = errors.New("lost connection to the director (was it removed?)")

// Run mirrors the director's run JSON. Token is always empty: the director redacts it.
type Run struct {
	ID       string    `json:"id"`
	Token    string    `json:"token,omitempty"`
	Persona  string    `json:"persona"`
	Role     string    `json:"role"`
	Story    string    `json:"story"`
	State    string    `json:"state"`
	Outcome  string    `json:"outcome"`
	Session  string    `json:"session"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// Bug mirrors the director's bug JSON.
type Bug struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Step     string `json:"step"`
	Evidence string `json:"evidence"`
	Severity string `json:"severity"`
	Note     string `json:"note"`
}

// RunDetail is the GET /api/runs/{id} response.
type RunDetail struct {
	Run       Run      `json:"run"`
	Bugs      []Bug    `json:"bugs"`
	Artifacts []string `json:"artifacts"`
}

// client talks to the director admin API.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 5 * time.Minute}}
}

func (c *client) do(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The director is reached through a local port-forward, so a transport
		// failure means the forward died, usually because the pod went away.
		var ne net.Error
		if ctx.Err() == nil && !(errors.As(err, &ne) && ne.Timeout()) {
			return nil, fmt.Errorf("%w: %v", errLostDirector, clean(err.Error()))
		}
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("director: %s: %s", clean(resp.Status), clean(strings.TrimSpace(string(msg))))
	}
	return resp, nil
}

func (c *client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// PutProject uploads project YAML.
func (c *client) PutProject(ctx context.Context, yaml []byte) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/project", bytes.NewReader(yaml), "application/yaml")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// StartRun starts a run and returns it (the token is redacted by the director).
func (c *client) StartRun(ctx context.Context, persona, story string) (Run, error) {
	b, _ := json.Marshal(map[string]string{"persona": persona, "story": story})
	resp, err := c.do(ctx, http.MethodPost, "/api/runs", bytes.NewReader(b), "application/json")
	if err != nil {
		return Run{}, err
	}
	defer resp.Body.Close()
	var r Run
	return r, json.NewDecoder(resp.Body).Decode(&r)
}

// Runs lists recent runs.
func (c *client) Runs(ctx context.Context) ([]Run, error) {
	var rs []Run
	return rs, c.getJSON(ctx, "/api/runs", &rs)
}

// RunDetail fetches one run with bugs and artifact names.
func (c *client) RunDetail(ctx context.Context, id string) (RunDetail, error) {
	var d RunDetail
	return d, c.getJSON(ctx, "/api/runs/"+url.PathEscape(id), &d)
}

// Artifact opens one artifact for reading.
func (c *client) Artifact(ctx context.Context, id, name string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(id)+"/artifacts/"+url.PathEscape(name), nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
