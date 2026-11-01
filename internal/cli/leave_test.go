package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1 "github.com/tiny-systems/tiny/api/v1alpha1"
)

const leaveNS = "bp"

var managedLabels = map[string]string{"app.kubernetes.io/managed-by": "boardingparty", "app.kubernetes.io/name": "bp-director"}

func om(name string, labelled bool) metav1.ObjectMeta {
	m := metav1.ObjectMeta{Name: name, Namespace: leaveNS}
	if labelled {
		m.Labels = managedLabels
	}
	return m
}

func directorObjs(labelled bool) []runtime.Object {
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: om("bp-director", labelled)},
		&corev1.Service{ObjectMeta: om("bp-director", labelled)},
		&rbacv1.RoleBinding{ObjectMeta: om("bp-director", labelled)},
		&rbacv1.Role{ObjectMeta: om("bp-director", labelled)},
		&corev1.ServiceAccount{ObjectMeta: om("bp-director", labelled)},
		&corev1.PersistentVolumeClaim{ObjectMeta: om("bp-director-data", labelled)},
	}
}

func (e *leaveEnv) log() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.order...)
}

func (e *leaveEnv) reset() {
	e.mu.Lock()
	e.order = nil
	e.mu.Unlock()
}

type leaveEnv struct {
	d     leaveDeps
	kube  *kubefake.Clientset
	ctrl  ctrlclient.Client
	mu    sync.Mutex
	order []string // "session:<name>" and "kube:<resource>/<name>" in delete order
}

func newLeaveEnv(t *testing.T, kubeObjs []runtime.Object, sessions ...*agentsv1.Session) *leaveEnv {
	t.Helper()
	e := &leaveEnv{}
	e.kube = kubefake.NewSimpleClientset(kubeObjs...)
	e.kube.PrependReactor("delete", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		da := a.(ktesting.DeleteAction)
		e.mu.Lock()
		e.order = append(e.order, "kube:"+a.GetResource().Resource+"/"+da.GetName())
		e.mu.Unlock()
		return false, nil, nil
	})
	e.kube.PrependReactor("update", "deployments", func(a ktesting.Action) (bool, runtime.Object, error) {
		e.mu.Lock()
		e.order = append(e.order, "scale:"+a.(ktesting.UpdateAction).GetObject().(*appsv1.Deployment).Name)
		e.mu.Unlock()
		go func() {
			time.Sleep(5 * time.Millisecond)
			_ = e.kube.CoreV1().Pods(leaveNS).Delete(context.Background(), "bp-director-pod", metav1.DeleteOptions{})
		}()
		return false, nil, nil
	})
	sc := runtime.NewScheme()
	if err := agentsv1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	b := ctrlfake.NewClientBuilder().WithScheme(sc).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
			do := &ctrlclient.DeleteOptions{}
			do.ApplyOptions(opts)
			if do.PropagationPolicy == nil || *do.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Errorf("session delete without foreground propagation")
			}
			e.mu.Lock()
			e.order = append(e.order, "session:"+obj.GetName())
			e.mu.Unlock()
			// The pod goes away shortly after its session, as with real garbage collection.
			go func() {
				time.Sleep(5 * time.Millisecond)
				_ = e.kube.CoreV1().Pods(leaveNS).Delete(context.Background(), obj.GetName()+"-pod", metav1.DeleteOptions{})
			}()
			return c.Delete(ctx, obj, opts...)
		},
	})
	for _, s := range sessions {
		b = b.WithObjects(s)
	}
	e.ctrl = b.Build()
	e.d = leaveDeps{kube: e.kube, ctrl: e.ctrl, poll: 2 * time.Millisecond}
	return e
}

func runSession(name string) *agentsv1.Session {
	return &agentsv1.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: leaveNS, Labels: map[string]string{"boardingparty.io/run": "x"}}}
}

func runPod(session string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: session + "-pod", Namespace: leaveNS,
		Labels: map[string]string{"app": "tiny-session", "tinysystems.io/session": session}}}
}

func opts(yes, keep bool) leaveOpts {
	return leaveOpts{ns: leaveNS, yes: yes, keepData: keep, interactive: false, timeout: 2 * time.Second, in: strings.NewReader("")}
}

func exists(t *testing.T, e *leaveEnv, group, res, name string) bool {
	t.Helper()
	var err error
	ctx := context.Background()
	switch res {
	case "deployments":
		_, err = e.kube.AppsV1().Deployments(leaveNS).Get(ctx, name, metav1.GetOptions{})
	case "services":
		_, err = e.kube.CoreV1().Services(leaveNS).Get(ctx, name, metav1.GetOptions{})
	case "rolebindings":
		_, err = e.kube.RbacV1().RoleBindings(leaveNS).Get(ctx, name, metav1.GetOptions{})
	case "roles":
		_, err = e.kube.RbacV1().Roles(leaveNS).Get(ctx, name, metav1.GetOptions{})
	case "serviceaccounts":
		_, err = e.kube.CoreV1().ServiceAccounts(leaveNS).Get(ctx, name, metav1.GetOptions{})
	case "persistentvolumeclaims":
		_, err = e.kube.CoreV1().PersistentVolumeClaims(leaveNS).Get(ctx, name, metav1.GetOptions{})
	}
	_ = group
	if err != nil && !errors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

var allSix = [][3]string{
	{"apps", "deployments", "bp-director"}, {"", "services", "bp-director"}, {"rbac", "rolebindings", "bp-director"},
	{"rbac", "roles", "bp-director"}, {"", "serviceaccounts", "bp-director"}, {"", "persistentvolumeclaims", "bp-director-data"},
}

func TestLeaveStopsRunsThenRemovesDirector(t *testing.T) {
	dpod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bp-director-pod", Namespace: leaveNS, Labels: map[string]string{"app": "bp-director"}}}
	objs := append(directorObjs(true), runPod("bp-run-1"), dpod)
	e := newLeaveEnv(t, objs, runSession("bp-run-1"))
	var out bytes.Buffer
	if err := runLeave(context.Background(), e.d, opts(true, false), &out); err != nil {
		t.Fatal(err)
	}
	for _, o := range allSix {
		if exists(t, e, o[0], o[1], o[2]) {
			t.Errorf("%s/%s still exists", o[1], o[2])
		}
	}
	var sl agentsv1.SessionList
	if err := e.ctrl.List(context.Background(), &sl); err != nil || len(sl.Items) != 0 {
		t.Errorf("sessions left: %v %v", sl.Items, err)
	}
	if l := e.log(); len(l) < 3 || l[0] != "scale:bp-director" || l[1] != "kube:pods/bp-director-pod" || l[2] != "session:bp-run-1" {
		t.Fatalf("want scale, director pod gone, then session: %v", l)
	}
	// The run pod must be gone before the first director object is deleted.
	first := ""
	for _, o := range e.log() {
		if strings.HasPrefix(o, "kube:") && !strings.HasPrefix(o, "kube:pods/") {
			first = o
			break
		}
	}
	if first == "" {
		t.Fatalf("no director deletes recorded: %v", e.log())
	}
	podIdx, firstIdx := -1, -1
	for i, o := range e.log() {
		if o == "kube:pods/bp-run-1-pod" {
			podIdx = i
		}
		if o == first && firstIdx < 0 {
			firstIdx = i
		}
	}
	if podIdx >= 0 && podIdx > firstIdx {
		t.Errorf("director deleted before run pod went: %v", e.log())
	}
	for _, want := range []string{"session bp-run-1", "deployment bp-director", "persistentvolumeclaim bp-director-data"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLeaveKeepData(t *testing.T) {
	e := newLeaveEnv(t, directorObjs(true))
	if err := runLeave(context.Background(), e.d, opts(true, true), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, e, "", "persistentvolumeclaims", "bp-director-data") {
		t.Error("PVC deleted despite --keep-data")
	}
	if exists(t, e, "apps", "deployments", "bp-director") {
		t.Error("deployment still exists")
	}
}

func TestLeaveLegacyUnlabelled(t *testing.T) {
	e := newLeaveEnv(t, directorObjs(false))
	if err := runLeave(context.Background(), e.d, opts(true, false), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range allSix {
		if exists(t, e, o[0], o[1], o[2]) {
			t.Errorf("%s/%s still exists", o[1], o[2])
		}
	}
}

func TestLeaveLeavesForeignObjects(t *testing.T) {
	other := &corev1.ServiceAccount{ObjectMeta: om("tiny-session", false)}
	e := newLeaveEnv(t, append(directorObjs(true), other))
	if err := runLeave(context.Background(), e.d, opts(true, false), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, e, "", "serviceaccounts", "tiny-session") {
		t.Error("tiny's service account was deleted")
	}
}

func TestLeaveNonTTYNeedsYes(t *testing.T) {
	e := newLeaveEnv(t, directorObjs(true), runSession("bp-run-1"))
	err := runLeave(context.Background(), e.d, opts(false, false), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("want --yes error, got %v", err)
	}
	if len(e.log()) != 0 {
		t.Errorf("deleted something: %v", e.log())
	}
}

func TestLeaveConfirmPrompt(t *testing.T) {
	for in, wantDeleted := range map[string]bool{"y\n": true, "n\n": false, "\n": false} {
		e := newLeaveEnv(t, directorObjs(true))
		o := opts(false, false)
		o.interactive, o.in = true, strings.NewReader(in)
		var out bytes.Buffer
		if err := runLeave(context.Background(), e.d, o, &out); err != nil {
			t.Fatal(err)
		}
		if got := !exists(t, e, "apps", "deployments", "bp-director"); got != wantDeleted {
			t.Errorf("answer %q: deleted=%v want %v", in, got, wantDeleted)
		}
	}
}

func TestLeaveIdempotent(t *testing.T) {
	e := newLeaveEnv(t, directorObjs(true), runSession("bp-run-1"))
	if err := runLeave(context.Background(), e.d, opts(true, false), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	e.reset()
	var out bytes.Buffer
	if err := runLeave(context.Background(), e.d, opts(false, false), &out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to remove") || len(e.log()) != 0 {
		t.Errorf("second run not a no-op: %q %v", out.String(), e.log())
	}
}

func TestLeaveWaitTimesOut(t *testing.T) {
	// A lingering run pod with no session to trigger its removal.
	e := newLeaveEnv(t, append(directorObjs(true), runPod("bp-run-9")))
	o := opts(true, false)
	o.timeout = 30 * time.Millisecond
	err := runLeave(context.Background(), e.d, o, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout, got %v", err)
	}
	if !exists(t, e, "apps", "deployments", "bp-director") {
		t.Error("director deleted although run pods remain")
	}
}
