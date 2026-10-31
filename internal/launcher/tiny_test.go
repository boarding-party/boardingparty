package launcher

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tiny-systems/boardingparty/internal/project"
	"github.com/tiny-systems/boardingparty/internal/store"
)

func newTiny() *Tiny {
	return &Tiny{
		Client:      fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
		Namespace:   "bp",
		WorkerImage: "ghcr.io/x/worker:1",
		DirectorURL: "http://bp-director:8080",
		Model:       "claude-sonnet-5-5",
	}
}

func getSession(t *testing.T, tn *Tiny, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(sessionGVK)
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
	if u.GetLabels()["boardingparty.io/run"] != r.ID {
		t.Errorf("labels = %v", u.GetLabels())
	}
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	if spec["image"] != "ghcr.io/x/worker:1" || spec["model"] != "claude-sonnet-5-5" {
		t.Errorf("spec = %v", spec)
	}
	if task, _ := spec["task"].(string); task != Prompt(r) {
		t.Errorf("task mismatch")
	}
	servers, _, _ := unstructured.NestedSlice(u.Object, "spec", "mcpServers")
	if len(servers) != 2 {
		t.Fatalf("servers = %v", servers)
	}
	bp := servers[0].(map[string]any)
	if bp["name"] != "bp" || bp["url"] != "http://bp-director:8080/mcp/run/tok123" {
		t.Errorf("bp = %v", bp)
	}
	pw := servers[1].(map[string]any)
	if pw["name"] != "pw" || pw["command"] != "playwright-mcp" {
		t.Errorf("pw = %v", pw)
	}
	args, _ := pw["args"].([]any)
	want := []string{"--headless", "--isolated", "--no-sandbox", "--browser=chromium",
		"--output-dir=/workspace/out", "--viewport-size=1280x800", "--caps=devtools"}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %v, want %s", i, args[i], w)
		}
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

func TestPhaseReadsStatus(t *testing.T) {
	tn := newTiny()
	name, _ := tn.Start(context.Background(), store.Run{ID: "bbbbbbbb11", Token: "t"}, project.Project{})
	u := getSession(t, tn, name)
	_ = unstructured.SetNestedField(u.Object, "Failed", "status", "phase")
	_ = unstructured.SetNestedField(u.Object, "boom", "status", "message")
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
		"browser_stop_video", "browser_stop_tracing", "next_assignment", "persona_save", "report_bug", "curl -T"} {
		if !strings.Contains(p, s) {
			t.Errorf("prompt missing %q", s)
		}
	}
}
