package artifacts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutRejectsBadNames(t *testing.T) {
	s := NewFS(t.TempDir(), 1024)
	for _, name := range []string{"../x", "a/b", "", strings.Repeat("a", 65), "Upper"} {
		if _, err := s.Put("run1", name, strings.NewReader("x")); !errors.Is(err, ErrBadName) {
			t.Errorf("name %q: want ErrBadName, got %v", name, err)
		}
	}
	if _, err := s.Put("../run", "ok", strings.NewReader("x")); !errors.Is(err, ErrBadName) {
		t.Errorf("bad run id: want ErrBadName, got %v", err)
	}
}

func TestPutTooLarge(t *testing.T) {
	root := t.TempDir()
	s := NewFS(root, 10)
	_, err := s.Put("run1", "big.png", strings.NewReader(strings.Repeat("x", 11)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	var left []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Fatalf("files left behind: %v", left)
	}
}

func TestPutOpenList(t *testing.T) {
	s := NewFS(t.TempDir(), 10)
	if n, err := s.Put("run1", "a.txt", strings.NewReader("hello")); err != nil || n != 5 {
		t.Fatalf("put: %d %v", n, err)
	}
	rc, err := s.Open("run1", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	names, err := s.List("run1")
	if err != nil || len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("list: %v %v", names, err)
	}
	if names, err := s.List("nope"); err != nil || len(names) != 0 {
		t.Fatalf("list empty: %v %v", names, err)
	}
	if _, err := s.Open("run1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListSkipsInFlightTempFiles(t *testing.T) {
	root := t.TempDir()
	s := NewFS(root, 10)
	s.Put("run1", "a.txt", strings.NewReader("x"))
	os.WriteFile(filepath.Join(root, "run1", ".upload-123"), []byte("partial"), 0o644)
	names, _ := s.List("run1")
	if len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("list: %v", names)
	}
}
