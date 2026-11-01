package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestOrderNamespacesReadyFirst(t *testing.T) {
	got := orderNamespaces([]string{"zeta", "default", "bp-b", "kube-system", "bp-a"}, map[string]bool{"zeta": true, "bp-b": true})
	want := []nsItem{{"bp-b", true}, {"zeta", true}, {"bp-a", false}, {"default", false}, {"kube-system", false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestCursors(t *testing.T) {
	ctxs := []string{"a", "b", "c"}
	if i := contextCursor(ctxs, "b", target{}); i != 1 {
		t.Errorf("current: %d", i)
	}
	if i := contextCursor(ctxs, "b", target{Context: "c"}); i != 2 {
		t.Errorf("remembered wins: %d", i)
	}
	if i := contextCursor(ctxs, "gone", target{Context: "also-gone"}); i != 0 {
		t.Errorf("fallback: %d", i)
	}
	items := []nsItem{{"x", true}, {"y", false}}
	if i := namespaceCursor(items, "a", target{Context: "a", Namespace: "y"}); i != 1 {
		t.Errorf("remembered ns: %d", i)
	}
	if i := namespaceCursor(items, "b", target{Context: "a", Namespace: "y"}); i != 0 {
		t.Errorf("other context must not preselect: %d", i)
	}
	if i := namespaceCursor(items, "a", target{Context: "a", Namespace: "deleted"}); i != 0 {
		t.Errorf("vanished ns: %d", i)
	}
}

func TestTargetFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := targetPath
	targetPath = func() (string, error) { return filepath.Join(dir, "boardingparty", "target.json"), nil }
	t.Cleanup(func() { targetPath = old })
	if got := loadTarget(); got != (target{}) {
		t.Errorf("empty: %v", got)
	}
	want := target{Context: "gke", Namespace: "bp"}
	if err := saveTarget(want); err != nil {
		t.Fatal(err)
	}
	if got := loadTarget(); got != want {
		t.Errorf("got %v", got)
	}
}

func TestListNamespacesMarksTinyReady(t *testing.T) {
	ns := func(n string) *corev1.Namespace { return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}} }
	sa := func(n, name string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: n, Name: name}}
	}
	kc := kubefake.NewSimpleClientset(ns("a"), ns("b"), ns("c"), sa("c", "tiny-session"), sa("a", "default"))
	items, err := listNamespaces(context.Background(), kc)
	if err != nil {
		t.Fatal(err)
	}
	want := []nsItem{{"c", true}, {"a", false}, {"b", false}}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("got %v", items)
	}
}

type fakePrompt struct {
	ctxAnswer, nsAnswer string
	err                 error
	calls               []string
	gotCtxCursor        int
	gotItems            []nsItem
	gotNsCursor         int
	gotNote             string
}

func (f *fakePrompt) pickContext(c []string, cursor int) (string, error) {
	f.calls = append(f.calls, "ctx")
	f.gotCtxCursor = cursor
	return f.ctxAnswer, f.err
}

func (f *fakePrompt) pickNamespace(_ string, items []nsItem, cursor int, note string) (string, error) {
	f.calls = append(f.calls, "ns")
	f.gotItems, f.gotNsCursor, f.gotNote = items, cursor, note
	return f.nsAnswer, f.err
}

type env struct {
	deps  targetDeps
	p     *fakePrompt
	out   *bytes.Buffer
	saved *target
}

func newEnv(interactive bool, contexts []string, current string, remembered target) *env {
	e := &env{p: &fakePrompt{ctxAnswer: "b", nsAnswer: "bp"}, out: &bytes.Buffer{}}
	e.deps = targetDeps{
		interactive: interactive,
		contexts:    func() ([]string, string, error) { return contexts, current, nil },
		namespaces: func(context.Context, string) ([]nsItem, error) {
			return []nsItem{{"bp", true}, {"default", false}}, nil
		},
		prompt: e.p, load: func() target { return remembered },
		save: func(t target) error { e.saved = &t; return nil }, out: e.out,
	}
	return e
}

func TestNonTTYNeverPromptsAndRequiresNamespace(t *testing.T) {
	e := newEnv(false, []string{"a", "b"}, "a", target{Context: "b", Namespace: "remembered"})
	_, err := resolveTarget(context.Background(), e.deps, "", "")
	if err == nil || !strings.Contains(err.Error(), "pass -n <namespace>") {
		t.Fatalf("err = %v", err)
	}
	got, err := resolveTarget(context.Background(), e.deps, "", "ci-ns")
	if err != nil {
		t.Fatal(err)
	}
	if got != (target{Context: "a", Namespace: "ci-ns"}) {
		t.Errorf("got %v (must use current context, not remembered)", got)
	}
	got, _ = resolveTarget(context.Background(), e.deps, "b", "ci-ns")
	if got.Context != "b" || len(e.p.calls) != 0 || e.saved != nil {
		t.Errorf("got %v calls %v saved %v", got, e.p.calls, e.saved)
	}
	if !strings.Contains(e.out.String(), "target: b / ci-ns") {
		t.Errorf("out = %q", e.out.String())
	}
}

func TestInteractiveAsksBothAndRemembers(t *testing.T) {
	e := newEnv(true, []string{"a", "b"}, "a", target{Context: "b", Namespace: "default"})
	got, err := resolveTarget(context.Background(), e.deps, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != (target{"b", "bp"}) || !reflect.DeepEqual(e.p.calls, []string{"ctx", "ns"}) {
		t.Errorf("got %v calls %v", got, e.p.calls)
	}
	if e.p.gotCtxCursor != 1 || e.p.gotNsCursor != 1 {
		t.Errorf("cursors ctx=%d ns=%d, want remembered preselected", e.p.gotCtxCursor, e.p.gotNsCursor)
	}
	if e.saved == nil || *e.saved != got {
		t.Errorf("saved = %v", e.saved)
	}
	if e.out.String() != "target: b / bp\n" {
		t.Errorf("out = %q", e.out.String())
	}
}

func TestInteractiveSkipsWhatFlagsOrSingleContextAnswer(t *testing.T) {
	e := newEnv(true, []string{"only"}, "only", target{})
	got, err := resolveTarget(context.Background(), e.deps, "", "")
	if err != nil || got.Context != "only" || !reflect.DeepEqual(e.p.calls, []string{"ns"}) {
		t.Errorf("single context: %v %v calls %v", got, err, e.p.calls)
	}
	e = newEnv(true, []string{"a", "b"}, "a", target{})
	got, _ = resolveTarget(context.Background(), e.deps, "b", "given")
	if got != (target{"b", "given"}) || len(e.p.calls) != 0 || e.saved == nil || *e.saved != got {
		t.Errorf("flags: %v calls %v saved %v (flags in a TTY are remembered)", got, e.p.calls, e.saved)
	}
	e = newEnv(true, []string{"a", "b"}, "a", target{})
	if _, _ = resolveTarget(context.Background(), e.deps, "b", ""); !reflect.DeepEqual(e.p.calls, []string{"ns"}) {
		t.Errorf("only -context: calls %v", e.p.calls)
	}
}

func TestListingFailureFallsBackToTyping(t *testing.T) {
	e := newEnv(true, []string{"a"}, "a", target{})
	e.deps.namespaces = func(context.Context, string) ([]nsItem, error) {
		return nil, errors.New(`namespaces is forbidden: User "x" cannot list`)
	}
	if _, err := resolveTarget(context.Background(), e.deps, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(e.p.gotItems) != 0 || !strings.Contains(e.p.gotNote, "forbidden") {
		t.Errorf("items %v note %q", e.p.gotItems, e.p.gotNote)
	}
}

func TestCancelChangesNothing(t *testing.T) {
	e := newEnv(true, []string{"a", "b"}, "a", target{})
	e.p.err = errCancelled
	_, err := resolveTarget(context.Background(), e.deps, "", "")
	if !errors.Is(err, errCancelled) || e.saved != nil || e.out.Len() != 0 {
		t.Errorf("err %v saved %v out %q", err, e.saved, e.out.String())
	}
}

func TestNoContextsIsAnError(t *testing.T) {
	e := newEnv(true, nil, "", target{})
	if _, err := resolveTarget(context.Background(), e.deps, "", ""); err == nil {
		t.Fatal("expected error")
	}
}

func keys(m tea.Model, ks ...tea.KeyMsg) pickModel {
	for _, k := range ks {
		m, _ = m.Update(k)
	}
	return m.(pickModel)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestPickModelNavigationAndChoice(t *testing.T) {
	m := newPickModel("t", "", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 1, false)
	m = keys(m, key("down"), key("down"), key("down"), key("up"), key("down"), key("enter"))
	if m.chosen != "c" || m.aborted {
		t.Errorf("chosen %q", m.chosen)
	}
}

func TestPickModelEscAborts(t *testing.T) {
	m := keys(newPickModel("t", "", []string{"a"}, []string{"a"}, 0, false), key("esc"))
	if !m.aborted || m.chosen != "" {
		t.Errorf("%+v", m)
	}
}

func TestPickModelTypeAName(t *testing.T) {
	m := newPickModel("t", "", []string{"a"}, []string{"a"}, 0, true)
	m = keys(m, key("down"), key("enter"))
	if !m.typing {
		t.Fatal("not typing")
	}
	m = keys(m, key("m"), key("y"), key("-"), key("n"), key("s"), key("enter"))
	if m.chosen != "my-ns" {
		t.Errorf("chosen %q", m.chosen)
	}
}

func TestPickModelEmptyListStartsTyping(t *testing.T) {
	m := newPickModel("t", "n", nil, nil, 0, true)
	if !m.typing {
		t.Error("expected typing mode")
	}
	if m = keys(m, key("enter")); m.chosen != "" || m.aborted {
		t.Error("empty name must not be accepted")
	}
}

func TestLostConnectionMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"state":"running"}`)) }))
	c := newClient(srv.URL)
	srv.Close() // the forward is gone
	start := time.Now()
	_, err := waitRun(context.Background(), c, "r1", time.Millisecond)
	if !errors.Is(err, errLostDirector) || !strings.Contains(err.Error(), "lost connection to the director (was it removed?): ") {
		t.Fatalf("err = %v (cause must stay visible)", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("too slow")
	}
}

func TestWindow(t *testing.T) {
	cases := []struct{ cursor, start, end int }{
		{0, 0, 10}, {25, 20, 30}, {49, 40, 50}, {3, 0, 10}, {46, 40, 50},
	}
	for _, c := range cases {
		s, e := window(50, c.cursor, 10)
		if s != c.start || e != c.end || c.cursor < s || c.cursor >= e {
			t.Errorf("cursor %d: got [%d,%d) want [%d,%d)", c.cursor, s, e, c.start, c.end)
		}
	}
	if s, e := window(5, 2, 10); s != 0 || e != 5 {
		t.Errorf("short list: [%d,%d)", s, e)
	}
	if s, e := window(50, 20, 0); s != 0 || e != 50 {
		t.Errorf("unknown height: [%d,%d)", s, e)
	}
}

func TestPickModelViewScrollsWithCursor(t *testing.T) {
	var l []string
	for i := 0; i < 50; i++ {
		l = append(l, fmt.Sprintf("ns-%02d", i))
	}
	var m tea.Model = newPickModel("t", "", l, l, 30, false)
	m, _ = m.Update(tea.WindowSizeMsg{Height: 16})
	v := m.View()
	if !strings.Contains(v, "ns-30") || strings.Contains(v, "ns-00") || !strings.Contains(v, "more") {
		t.Errorf("view:\n%s", v)
	}
	if n := strings.Count(v, "\n"); n > 16 {
		t.Errorf("view is %d lines for a 16-row terminal", n)
	}
}

func TestValidNamespace(t *testing.T) {
	for n, want := range map[string]bool{"bp": true, "a-b1": true, "Bad": false, "-x": false, "x-": false, "a_b": false, strings.Repeat("a", 64): false} {
		if validNamespace(n) != want {
			t.Errorf("%q: want %v", n, want)
		}
	}
}

func TestTypingRejectsInvalidAndEscGoesBack(t *testing.T) {
	m := newPickModel("t", "", []string{"a"}, []string{"a"}, 0, true)
	m = keys(m, key("down"), key("enter"), key("B"), key("enter"))
	if m.chosen != "" || !m.typing || m.errMsg == "" {
		t.Fatalf("invalid name accepted: %+v", m)
	}
	m = keys(m, key("esc"))
	if m.typing || m.aborted {
		t.Errorf("esc while typing must return to the list: %+v", m)
	}
	m = keys(m, key("esc"))
	if !m.aborted {
		t.Error("esc on the list must cancel")
	}
	only := keys(newPickModel("t", "", nil, nil, 0, true), key("esc"))
	if !only.aborted {
		t.Error("esc with nothing to go back to must cancel")
	}
}

func TestChooseTargetFlagsNonTTY(t *testing.T) {
	old := isTerminal
	isTerminal = func() bool { return false }
	t.Cleanup(func() { isTerminal = old })
	g := &globals{kubeContext: "ctx-x", namespace: "ns-x"}
	if err := g.chooseTarget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.kubeContext != "ctx-x" || g.namespace != "ns-x" || !g.resolved {
		t.Errorf("globals = %+v", g)
	}
	g = &globals{}
	if err := g.chooseTarget(context.Background()); err == nil || !strings.Contains(err.Error(), "-n") {
		t.Errorf("err = %v", err)
	}
}
