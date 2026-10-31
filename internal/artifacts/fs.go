// Package artifacts stores files (screenshots, traces) uploaded by workers.
package artifacts

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var (
	ErrTooLarge = errors.New("artifact too large")
	ErrBadName  = errors.New("bad artifact name")
	ErrNotFound = errors.New("artifact not found")
)

var namePattern = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// validName also rejects "." and ".." which the pattern alone would allow.
func validName(n string) bool {
	return namePattern.MatchString(n) && n != "." && n != ".."
}

type Store interface {
	Put(runID, name string, r io.Reader) (int64, error)
	Open(runID, name string) (io.ReadCloser, error)
	List(runID string) ([]string, error)
}

type fs struct {
	root     string
	maxBytes int64
}

// NewFS stores artifacts under root/<runID>/<name>, each at most maxBytes.
func NewFS(root string, maxBytes int64) Store { return &fs{root: root, maxBytes: maxBytes} }

func (f *fs) Put(runID, name string, r io.Reader) (int64, error) {
	if !validName(runID) || !validName(name) {
		return 0, ErrBadName
	}
	dir := filepath.Join(f.root, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	n, err := io.Copy(tmp, io.LimitReader(r, f.maxBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return 0, ErrTooLarge
	}
	if err != nil {
		return 0, err
	}
	if n > f.maxBytes {
		return 0, ErrTooLarge
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return 0, err
	}
	return n, nil
}

func (f *fs) Open(runID, name string) (io.ReadCloser, error) {
	if !validName(runID) || !validName(name) {
		return nil, ErrBadName
	}
	file, err := os.Open(filepath.Join(f.root, runID, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (f *fs) List(runID string) ([]string, error) {
	if !validName(runID) {
		return nil, ErrBadName
	}
	entries, err := os.ReadDir(filepath.Join(f.root, runID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && validName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
