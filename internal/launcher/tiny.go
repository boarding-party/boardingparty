// Package launcher runs each persona run as one tiny Session.
package launcher

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/store"
)

const runLabel = "boardingparty.io/run"

var sessionGVK = schema.GroupVersionKind{Group: "agents.tinysystems.io", Version: "v1alpha1", Kind: "Session"}

// Tiny launches runs as tiny Sessions. Sessions are handled as unstructured
// objects so this module does not depend on tiny's Go types.
type Tiny struct {
	Client      client.Client
	Namespace   string
	WorkerImage string
	DirectorURL string // in-namespace, e.g. http://bp-director:8080
	Model       string
}

// sessionName is bp-run-<first 8 of run ID>; run IDs are lower-case hex.
func sessionName(runID string) string {
	if len(runID) > 8 {
		runID = runID[:8]
	}
	return "bp-run-" + strings.ToLower(runID)
}

// SessionName implements runs.Launcher.
func (t *Tiny) SessionName(runID string) string { return sessionName(runID) }

func newSession() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sessionGVK)
	return u
}

func (t *Tiny) Start(ctx context.Context, r store.Run, p project.Project) (string, error) {
	model := t.Model
	if p.Model != "" {
		model = p.Model
	}
	name := sessionName(r.ID)
	u := newSession()
	u.SetNamespace(t.Namespace)
	u.SetName(name)
	u.SetLabels(map[string]string{runLabel: r.ID})
	u.Object["spec"] = map[string]any{
		"task":  Prompt(r),
		"image": t.WorkerImage,
		"model": model,
		"mcpServers": []any{
			map[string]any{"name": "bp", "url": t.DirectorURL + "/mcp/run/" + r.Token},
			map[string]any{
				"name":    "pw",
				"command": "playwright-mcp",
				"args": []any{"--headless", "--isolated", "--no-sandbox", "--browser=chromium",
					"--output-dir=/workspace/out", "--viewport-size=1280x800", "--caps=devtools"},
			},
		},
	}
	if err := t.Client.Create(ctx, u); err != nil {
		return "", fmt.Errorf("create session %s: %w", name, err)
	}
	return name, nil
}

// Stop deletes the session; a missing session is success.
func (t *Tiny) Stop(ctx context.Context, session string) error {
	u := newSession()
	u.SetNamespace(t.Namespace)
	u.SetName(session)
	if err := t.Client.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete session %s: %w", session, err)
	}
	return nil
}

// Phase returns the session's status phase and message, and whether it exists.
func (t *Tiny) Phase(ctx context.Context, session string) (string, bool, string, error) {
	u := newSession()
	err := t.Client.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: session}, u)
	if apierrors.IsNotFound(err) {
		return "", false, "", nil
	}
	if err != nil {
		return "", false, "", fmt.Errorf("get session %s: %w", session, err)
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	msg, _, _ := unstructured.NestedString(u.Object, "status", "message")
	return phase, true, msg, nil
}

// Sessions lists run sessions (those labelled boardingparty.io/run) as
// session name -> run ID.
func (t *Tiny) Sessions(ctx context.Context) (map[string]string, error) {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(schema.GroupVersionKind{Group: sessionGVK.Group, Version: sessionGVK.Version, Kind: "SessionList"})
	if err := t.Client.List(ctx, l, client.InNamespace(t.Namespace), client.HasLabels{runLabel}); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make(map[string]string, len(l.Items))
	for _, u := range l.Items {
		out[u.GetName()] = u.GetLabels()[runLabel]
	}
	return out, nil
}
