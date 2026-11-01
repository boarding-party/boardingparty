// Package launcher runs each persona run as one tiny Session.
package launcher

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1 "github.com/tiny-systems/tiny/api/v1alpha1"
	"github.com/tiny-systems/tiny/pkg/session"

	"github.com/boarding-party/boardingparty/internal/project"
	"github.com/boarding-party/boardingparty/internal/store"
)

const runLabel = "boardingparty.io/run"

// Labels tiny puts on a session's pods (internal/workload): every session pod
// has app=tiny-session and tinysystems.io/session=<session name>.
const (
	podAppLabel   = "app"
	podAppValue   = "tiny-session"
	runNamePrefix = "bp-run-"
)

// Tiny launches runs as tiny Sessions. tiny has no manager, so Start creates
// the Session and its workload (workspace PVC and Deployment) itself through
// tiny's pkg/session.
type Tiny struct {
	Client      client.Client // scheme must know corev1, appsv1 and agentsv1
	Namespace   string
	WorkerImage string
	DirectorURL string // in-namespace, with a .svc name, e.g. http://bp-director.<ns>.svc:8080
	Model       string
	// Owner, when set, is put on every Session so that deleting the owner (the
	// director's Deployment) garbage-collects the run Sessions and their pods.
	Owner *metav1.OwnerReference
}

// DirectorDeployment is the name of the director's own Deployment.
const DirectorDeployment = "bp-director"

// DirectorOwner reads the director's Deployment and returns an owner reference
// to it. blockOwnerDeletion is false and controller is unset: the reference
// only drives garbage collection.
func DirectorOwner(ctx context.Context, r client.Reader, namespace string) (*metav1.OwnerReference, error) {
	d := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: DirectorDeployment}, d); err != nil {
		return nil, fmt.Errorf("get deployment %s: %w", DirectorDeployment, err)
	}
	f := false
	return &metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID,
		BlockOwnerDeletion: &f, Controller: &f,
	}, nil
}

// sessionName is bp-run-<first 8 of run ID>; run IDs are lower-case hex.
func sessionName(runID string) string {
	if len(runID) > 8 {
		runID = runID[:8]
	}
	return runNamePrefix + strings.ToLower(runID)
}

// SessionName implements runs.Launcher.
func (t *Tiny) SessionName(runID string) string { return sessionName(runID) }

func (t *Tiny) Start(ctx context.Context, r store.Run, p project.Project) (string, error) {
	model := t.Model
	if p.Model != "" {
		model = p.Model
	}
	name := sessionName(r.ID)
	res := p.RunResources() // always set: a run pod is never unbounded
	s := &agentsv1.Session{
		ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: name, Labels: map[string]string{runLabel: r.ID}},
		Spec: agentsv1.SessionSpec{
			Task:      Prompt(r),
			Image:     t.WorkerImage,
			Model:     model,
			Resources: &agentsv1.SessionResources{CPU: res.CPU, Memory: res.Memory},
			MCPServers: []agentsv1.MCPServer{
				{Name: "bp", URL: t.DirectorURL + "/mcp/run/" + r.Token},
				{
					Name:    "pw",
					Command: "playwright-mcp",
					Args: []string{"--headless", "--isolated", "--no-sandbox", "--browser=chromium",
						"--output-dir=/workspace/out", "--viewport-size=1280x800", "--caps=devtools"},
				},
			},
		},
	}
	if t.Owner != nil {
		s.OwnerReferences = []metav1.OwnerReference{*t.Owner}
	}
	if err := session.Create(ctx, t.Client, s); err != nil {
		return "", fmt.Errorf("create session %s: %w", name, err)
	}
	return name, nil
}

// Stop deletes the session (its workload goes with it); a missing session is
// success. The delete is foreground: the Session stays listed, with a
// deletionTimestamp, until its Deployment, ReplicaSet and pods are gone, so the
// busy gate keeps seeing it. Re-issuing the delete on a deleting Session is a no-op.
func (t *Tiny) Stop(ctx context.Context, name string) error {
	s := &agentsv1.Session{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: name}}
	err := t.Client.Delete(ctx, s, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err = client.IgnoreNotFound(err); err != nil {
		return fmt.Errorf("delete session %s: %w", name, err)
	}
	return nil
}

// Phase returns the session's status phase and message, and whether it exists.
func (t *Tiny) Phase(ctx context.Context, name string) (string, bool, string, error) {
	s := &agentsv1.Session{}
	err := t.Client.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: name}, s)
	if apierrors.IsNotFound(err) {
		return "", false, "", nil
	}
	if err != nil {
		return "", false, "", fmt.Errorf("get session %s: %w", name, err)
	}
	return string(s.Status.Phase), true, s.Status.Message, nil
}

// Sessions lists run sessions (those labelled boardingparty.io/run) as
// session name -> run ID.
func (t *Tiny) Sessions(ctx context.Context) (map[string]string, error) {
	l := &agentsv1.SessionList{}
	if err := t.Client.List(ctx, l, client.InNamespace(t.Namespace), client.HasLabels{runLabel}); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make(map[string]string, len(l.Items))
	for _, s := range l.Items {
		out[s.Name] = s.Labels[runLabel]
	}
	return out, nil
}

// LivePods counts the pods of run sessions that still exist, terminating or
// not. Deleting a Session returns before its pod is gone (background
// garbage collection), and a lingering pod can read the next run's token.
func (t *Tiny) LivePods(ctx context.Context) (int, error) {
	l := &corev1.PodList{}
	if err := t.Client.List(ctx, l, client.InNamespace(t.Namespace),
		client.MatchingLabels{podAppLabel: podAppValue}, client.HasLabels{session.SessionLabel}); err != nil {
		return 0, fmt.Errorf("list pods: %w", err)
	}
	n := 0
	for _, p := range l.Items {
		if strings.HasPrefix(p.Labels[session.SessionLabel], runNamePrefix) {
			n++
		}
	}
	return n, nil
}
