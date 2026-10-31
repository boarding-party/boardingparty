// Package api exposes the director over HTTP.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/runs"
	"github.com/tiny-systems/boardingparty/internal/store"
)

// MaxArtifactBytes is the default per-artifact upload limit (500 MB).
const MaxArtifactBytes int64 = 500 << 20

type server struct {
	svc      *runs.Service
	maxBytes int64
}

// New returns the HTTP handler. maxBytes <= 0 means MaxArtifactBytes.
func New(svc *runs.Service, maxBytes int64) http.Handler {
	if maxBytes <= 0 {
		maxBytes = MaxArtifactBytes
	}
	s := &server{svc: svc, maxBytes: maxBytes}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/project", s.putProject)
	mux.HandleFunc("POST /api/runs", s.startRun)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/runs/{id}/artifacts/{name}", s.getArtifact)
	mux.HandleFunc("PUT /artifacts/{token}/{name}", s.putArtifact)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, artifacts.ErrNotFound), errors.Is(err, runs.ErrNotActive):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, artifacts.ErrBadName):
		http.Error(w, "bad name", http.StatusBadRequest)
	case errors.Is(err, artifacts.ErrTooLarge):
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *server) putProject(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	p, err := project.Load(b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.svc.Store.PutProject(p); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) startRun(w http.ResponseWriter, r *http.Request) {
	var req struct{ Persona, Story string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	run, err := s.svc.Start(r.Context(), req.Persona, req.Story)
	if err != nil {
		// Unknown persona/story is a client error; a launcher failure still
		// leaves a failed run behind (run.ID set).
		code := http.StatusBadRequest
		if run.ID != "" {
			code = http.StatusBadGateway
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// redact hides the worker token from read endpoints.
func redact(r store.Run) store.Run { r.Token = ""; return r }

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	rs, err := s.svc.Store.Runs(100)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]store.Run, len(rs))
	for i, x := range rs {
		out[i] = redact(x)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.svc.Store.RunByID(id)
	if err != nil {
		fail(w, err)
		return
	}
	bugs, err := s.svc.Store.Bugs(id)
	if err != nil {
		fail(w, err)
		return
	}
	names, err := s.svc.Artifacts.List(id)
	if err != nil {
		fail(w, err)
		return
	}
	if bugs == nil {
		bugs = []store.Bug{}
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Run": redact(run), "Bugs": bugs, "Artifacts": names})
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	rc, err := s.svc.Artifacts.Open(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, rc)
}

func (s *server) putArtifact(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.Active(r.PathValue("token"))
	if err != nil {
		fail(w, err)
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.maxBytes)
	if _, err := s.svc.Artifacts.Put(run.ID, r.PathValue("name"), body); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}
