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

func newSession() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sessionGVK)
	return u
}

func (t *Tiny) Start(ctx context.Context, r store.Run, _ project.Project) (string, error) {
	name := sessionName(r.ID)
	u := newSession()
	u.SetNamespace(t.Namespace)
	u.SetName(name)
	u.SetLabels(map[string]string{"boardingparty.io/run": r.ID})
	u.Object["spec"] = map[string]any{
		"task":  Prompt(r),
		"image": t.WorkerImage,
		"model": t.Model,
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
