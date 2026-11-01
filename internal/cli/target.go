package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// tinySessionSA is the ServiceAccount `tiny init` creates; its presence marks
// a namespace where tiny is set up.
const tinySessionSA = "tiny-session"

// errCancelled is returned when the human leaves a picker with esc or ctrl+c.
var errCancelled = errors.New("cancelled, nothing changed")

// target is where a command acts: a kubeconfig context and a namespace.
type target struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
}

// nsItem is one namespace in the picker; Ready means tiny is set up there.
type nsItem struct {
	Name  string
	Ready bool
}

// orderNamespaces puts namespaces where tiny is set up first, then the rest;
// each group is alphabetical.
func orderNamespaces(names []string, ready map[string]bool) []nsItem {
	items := make([]nsItem, 0, len(names))
	for _, n := range names {
		items = append(items, nsItem{Name: n, Ready: ready[n]})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Ready != items[j].Ready {
			return items[i].Ready
		}
		return items[i].Name < items[j].Name
	})
	return items
}

// contextCursor picks the preselected context: the remembered one, else the
// kubeconfig's current one, else the first.
func contextCursor(contexts []string, current string, remembered target) int {
	for _, want := range []string{remembered.Context, current} {
		for i, c := range contexts {
			if want != "" && c == want {
				return i
			}
		}
	}
	return 0
}

// namespaceCursor preselects the remembered namespace when it was remembered
// for this same context and still exists; otherwise the first item.
func namespaceCursor(items []nsItem, ctxName string, remembered target) int {
	if remembered.Context != ctxName {
		return 0
	}
	for i, it := range items {
		if it.Name == remembered.Namespace {
			return i
		}
	}
	return 0
}

// targetPath is where the last chosen target is remembered. A variable so
// tests can point it elsewhere.
var targetPath = func() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "boardingparty", "target.json"), nil
}

// loadTarget returns the remembered target; a missing or unreadable file is
// not an error, there is just nothing remembered.
func loadTarget() target {
	p, err := targetPath()
	if err != nil {
		return target{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return target{}
	}
	var t target
	if json.Unmarshal(b, &t) != nil {
		return target{}
	}
	return t
}

func saveTarget(t target) error {
	p, err := targetPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(t)
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// prompter asks the human. The terminal implementation is in picker.go.
type prompter interface {
	pickContext(contexts []string, cursor int) (string, error)
	// pickNamespace offers items plus "type a name". note explains why the
	// list is missing or short; items may be empty (typing only).
	pickNamespace(ctxName string, items []nsItem, cursor int, note string) (string, error)
}

// targetDeps is everything resolveTarget touches, so it can be tested.
type targetDeps struct {
	interactive bool
	contexts    func() (all []string, current string, err error)
	namespaces  func(ctx context.Context, kubeContext string) ([]nsItem, error)
	prompt      prompter
	load        func() target
	save        func(target) error
	out         io.Writer
}

// resolveTarget decides the (context, namespace) a command acts on. With a
// terminal it asks for whatever the flags did not give; without one it never
// asks and needs -n. It prints "target: <context> / <namespace>" either way.
func resolveTarget(ctx context.Context, d targetDeps, flagCtx, flagNS string) (target, error) {
	contexts, current, err := d.contexts()
	if err != nil {
		return target{}, fmt.Errorf("kube config: %w", err)
	}
	if !d.interactive {
		if flagNS == "" {
			return target{}, errors.New("not a terminal, so cannot ask which namespace: pass -n <namespace>")
		}
		t := target{Context: flagCtx, Namespace: flagNS}
		if t.Context == "" {
			t.Context = current
		}
		return d.announce(t), nil
	}

	remembered := d.load()
	t := target{Context: flagCtx, Namespace: flagNS}
	if t.Context == "" {
		switch len(contexts) {
		case 0:
			return target{}, errors.New("no kubeconfig contexts found: is kubectl configured?")
		case 1:
			t.Context = contexts[0]
		default:
			if t.Context, err = d.prompt.pickContext(contexts, contextCursor(contexts, current, remembered)); err != nil {
				return target{}, err
			}
		}
	}
	if t.Namespace == "" {
		items, lerr := d.namespaces(ctx, t.Context)
		note := ""
		if lerr != nil {
			items = nil
			note = "cannot list namespaces on " + t.Context + ": " + shortErr(lerr)
		}
		if t.Namespace, err = d.prompt.pickNamespace(t.Context, items, namespaceCursor(items, t.Context, remembered), note); err != nil {
			return target{}, err
		}
	}
	// Remembering is a convenience; a read-only config dir must not stop a run.
	if serr := d.save(t); serr != nil {
		fmt.Fprintf(d.out, "note: could not remember the target: %s\n", shortErr(serr))
	}
	return d.announce(t), nil
}

func (d targetDeps) announce(t target) target {
	fmt.Fprintf(d.out, "target: %s / %s\n", clean(t.Context), clean(t.Namespace))
	return t
}

func shortErr(err error) string {
	s := clean(err.Error())
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// kubeContexts lists the kubeconfig's context names (sorted) and the current one.
func kubeContexts() ([]string, string, error) {
	raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(raw.Contexts))
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, raw.CurrentContext, nil
}

// listNamespaces lists the cluster's namespaces, tiny-ready ones first. Failing
// to learn which are ready (e.g. no cluster-wide ServiceAccount list) only
// loses the marks; failing to list namespaces is an error.
func listNamespaces(ctx context.Context, kube kubernetes.Interface) ([]nsItem, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nl, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nl.Items))
	for _, n := range nl.Items {
		names = append(names, n.Name)
	}
	ready := map[string]bool{}
	sas, err := kube.CoreV1().ServiceAccounts(corev1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + tinySessionSA})
	if err == nil {
		for _, sa := range sas.Items {
			if sa.Name == tinySessionSA {
				ready[sa.Namespace] = true
			}
		}
	}
	return orderNamespaces(names, ready), nil
}
