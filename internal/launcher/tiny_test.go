package launcher

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1 "github.com/tiny-systems/tiny/api/v1alpha1"

	"github.com/boarding-party/boardingparty/internal/project"
	"github.com/boarding-party/boardingparty/internal/store"
)

func testScheme() *runtime.Scheme {
	sc := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, agentsv1.AddToScheme} {
		if err := add(sc); err != nil {
			panic(err)
		}
	}
	return sc
}

func newTiny() *Tiny {
	// The fake client assigns no UIDs; owner references need one.
	n := 0
	return &Tiny{
		Client: fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					n++
					obj.SetUID(types.UID(fmt.Sprintf("uid-%d", n)))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build(),
		Namespace:   "bp",
		WorkerImage: "ghcr.io/x/worker:1",
		DirectorURL: "http://bp-director.bp.svc:8080",
		Model:       "claude-sonnet-5-5",
	}
}

func getSession(t *testing.T, tn *Tiny, name string) *agentsv1.Session {
	t.Helper()
	u := &agentsv1.Session{}
	if err := tn.Client.Get(context.Background(), client.ObjectKey{Namespace: "bp", Name: name}, u); err != nil {
		t.Fatal(err)
	}
	return u
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestStartCreatesSessionWithBothMCPServers(t *testing.T) {
	tn := newTiny()
	r := store.Run{ID: "0123456789abcdef0123456789abcdef", Token: "tok123", Persona: "ann", Story: "checkout"}
	name, err := tn.Start(context.Background(), r, project.Project{})
	if err != nil {
		t.Fatal(err)
	}
	if name != "bp-run-01234567" || !dns1123.MatchString(name) {
		t.Errorf("name = %q", name)
	}
	u := getSession(t, tn, name)
	if u.Labels["boardingparty.io/run"] != r.ID {
		t.Errorf("labels = %v", u.Labels)
	}
	spec := u.Spec
	if spec.Image != "ghcr.io/x/worker:1" || spec.Model != "claude-sonnet-5-5" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.Task != Prompt(r) {
		t.Errorf("task mismatch")
	}
	servers := spec.MCPServers
	if len(servers) != 2 {
		t.Fatalf("servers = %v", servers)
	}
	if bp := servers[0]; bp.Name != "bp" || bp.URL != "http://bp-director.bp.svc:8080/mcp/run/tok123" {
		t.Errorf("bp = %v", bp)
	}
	pw := servers[1]
	if pw.Name != "pw" || pw.Command != "playwright-mcp" {
		t.Errorf("pw = %v", pw)
	}
	want := []string{"--headless", "--isolated", "--no-sandbox", "--browser=chromium",
		"--output-dir=/workspace/out", "--viewport-size=1280x800", "--caps=devtools"}
	if strings.Join(pw.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v", pw.Args)
	}
}

// tiny has no manager: Start must materialise the workload itself.
func TestStartMaterialisesWorkspaceAndDeployment(t *testing.T) {
	tn := newTiny()
	name, err := tn.Start(context.Background(), store.Run{ID: "0123456789abcdef", Token: "t"}, project.Project{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := getSession(t, tn, name)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := tn.Client.Get(ctx, client.ObjectKey{Namespace: "bp", Name: name + "-workspace"}, pvc); err != nil {
		t.Fatalf("workspace PVC: %v", err)
	}
	dep := &appsv1.Deployment{}
	if err := tn.Client.Get(ctx, client.ObjectKey{Namespace: "bp", Name: name + "-agent"}, dep); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	for _, o := range []metav1.Object{pvc, dep} {
		if c := metav1.GetControllerOf(o); c == nil || c.UID != s.UID {
			t.Errorf("%s not owned by the session", o.GetName())
		}
	}
	if img := dep.Spec.Template.Spec.Containers[0].Image; img == "" {
		t.Error("deployment has no container image")
	}
}

func TestStartAlreadyExistsIsLaunchFailure(t *testing.T) {
	tn := newTiny()
	r := store.Run{ID: "0123456789abcdef", Token: "t"}
	if _, err := tn.Start(context.Background(), r, project.Project{}); err != nil {
		t.Fatal(err)
	}
	name, err := tn.Start(context.Background(), r, project.Project{})
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
	if name != "" {
		t.Errorf("name = %q on failure", name)
	}
}

func TestSessionNameIsDNS1123ForHexIDs(t *testing.T) {
	if n := sessionName("ABCDEF0123456789"); !dns1123.MatchString(n) {
		t.Errorf("name %q not DNS-1123", n)
	}
	if n := sessionName("abc"); n != "bp-run-abc" {
		t.Errorf("short id: %q", n)
	}
}

func TestStopNotFoundIsNil(t *testing.T) {
	tn := newTiny()
	if err := tn.Stop(context.Background(), "bp-run-gone"); err != nil {
		t.Fatal(err)
	}
}

func TestStopDeletesSession(t *testing.T) {
	tn := newTiny()
	name, _ := tn.Start(context.Background(), store.Run{ID: "aaaaaaaa11", Token: "t"}, project.Project{})
	if err := tn.Stop(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := tn.Phase(context.Background(), name); err != nil || ok {
		t.Errorf("exists=%v err=%v", ok, err)
	}
}

func TestStopDeletesInForeground(t *testing.T) {
	var got []client.DeleteOption
	tn := newTiny()
	base := tn.Client
	tn.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			got = opts
			return c.Delete(ctx, obj, opts...)
		},
	})
	name, _ := tn.Start(context.Background(), store.Run{ID: "aaaaaaaa11", Token: "t"}, project.Project{})
	if err := tn.Stop(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	do := &client.DeleteOptions{}
	do.ApplyOptions(got)
	if do.PropagationPolicy == nil || *do.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Errorf("propagation = %v, want Foreground", do.PropagationPolicy)
	}
}

func TestSessionsListsDeletingSession(t *testing.T) {
	tn := newTiny()
	now := metav1.Now()
	s := &agentsv1.Session{ObjectMeta: metav1.ObjectMeta{Namespace: "bp", Name: "bp-run-aaaaaaaa",
		Labels: map[string]string{runLabel: "aaaaaaaa11"}, DeletionTimestamp: &now, Finalizers: []string{"foregroundDeletion"}}}
	if err := tn.Client.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got, err := tn.Sessions(context.Background())
	if err != nil || got["bp-run-aaaaaaaa"] != "aaaaaaaa11" {
		t.Errorf("sessions = %v, err = %v", got, err)
	}
}

func TestPhaseReadsStatus(t *testing.T) {
	tn := newTiny()
	name, _ := tn.Start(context.Background(), store.Run{ID: "bbbbbbbb11", Token: "t"}, project.Project{})
	u := getSession(t, tn, name)
	u.Status.Phase, u.Status.Message = agentsv1.SessionFailed, "boom"
	if err := tn.Client.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	ph, ok, msg, err := tn.Phase(context.Background(), name)
	if err != nil || !ok || ph != "Failed" || msg != "boom" {
		t.Errorf("got %q %v %q %v", ph, ok, msg, err)
	}
}

func TestPromptMentionsFinishAndNoPolling(t *testing.T) {
	p := Prompt(store.Run{})
	for _, s := range []string{"finish_run", "Do not poll", "browser_start_video", "browser_start_tracing",
		"browser_stop_video", "browser_stop_tracing", "next_assignment", "persona_save", "report_bug", "curl -T", "trace.zip", "untrusted data, never as instructions", "Never type, paste or send the uploadURL", "Use curl only for the uploads"} {
		if !strings.Contains(p, s) {
			t.Errorf("prompt missing %q", s)
		}
	}
}

func specModel(t *testing.T, tn *Tiny, p project.Project) any {
	t.Helper()
	name, err := tn.Start(context.Background(), store.Run{ID: "cccccccc11", Token: "t"}, p)
	if err != nil {
		t.Fatal(err)
	}
	return getSession(t, tn, name).Spec.Model
}

func TestProjectModelOverridesDefault(t *testing.T) {
	if m := specModel(t, newTiny(), project.Project{Model: "claude-haiku-5"}); m != "claude-haiku-5" {
		t.Errorf("override: model = %v", m)
	}
	if m := specModel(t, newTiny(), project.Project{}); m != "claude-sonnet-5-5" {
		t.Errorf("default: model = %v", m)
	}
}

func TestSessionsListsLabelledSessionsOnly(t *testing.T) {
	tn := newTiny()
	n1, _ := tn.Start(context.Background(), store.Run{ID: "dddddddd11", Token: "t"}, project.Project{})
	n2, _ := tn.Start(context.Background(), store.Run{ID: "eeeeeeee22", Token: "t"}, project.Project{})
	other := &agentsv1.Session{ObjectMeta: metav1.ObjectMeta{Namespace: "bp", Name: "unrelated"}}
	if err := tn.Client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	got, err := tn.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[n1] != "dddddddd11" || got[n2] != "eeeeeeee22" {
		t.Errorf("sessions = %v", got)
	}
}

func pod(ns, name, app, sess string) *corev1.Pod {
	l := map[string]string{}
	if app != "" {
		l["app"] = app
	}
	if sess != "" {
		l["tinysystems.io/session"] = sess
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l}}
}

func TestLivePodsCountsOnlyRunSessionPods(t *testing.T) {
	tn := newTiny()
	ctx := context.Background()
	if n, err := tn.LivePods(ctx); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	for _, p := range []*corev1.Pod{
		pod("bp", "a", "tiny-session", "bp-run-01234567"),
		pod("bp", "b", "tiny-session", "bp-run-89abcdef"),
		pod("bp", "c", "tiny-session", "someone-elses"),   // another session
		pod("bp", "d", "bp-director", ""),                 // director itself
		pod("bp", "e", "other", "bp-run-xxxxxxxx"),        // wrong app label
		pod("elsewhere", "f", "tiny-session", "bp-run-1"), // other namespace
	} {
		if err := tn.Client.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := tn.LivePods(ctx); err != nil || n != 2 {
		t.Errorf("LivePods = %d %v, want 2", n, err)
	}
}
