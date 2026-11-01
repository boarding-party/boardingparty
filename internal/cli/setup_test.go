package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/boarding-party/boardingparty/deploy"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const testNS = "bp-test"

func crdWith(mcp bool) *apiextv1.CustomResourceDefinition {
	spec := apiextv1.JSONSchemaProps{Properties: map[string]apiextv1.JSONSchemaProps{"prompt": {}}}
	if mcp {
		spec.Properties["mcpServers"] = apiextv1.JSONSchemaProps{}
	}
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: sessionCRD},
		Spec: apiextv1.CustomResourceDefinitionSpec{Versions: []apiextv1.CustomResourceDefinitionVersion{{
			Name: "v1",
			Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
				Properties: map[string]apiextv1.JSONSchemaProps{"spec": spec}}},
		}}},
	}
}

func secret(data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agentSecret, Namespace: testNS}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func cm(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}, Data: data}
}

var (
	nsObj = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}
	saObj = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sessionSA, Namespace: testNS}}
)

func TestPreflight(t *testing.T) {
	const apiKey = "sk-ant-SECRETVALUE"
	proxyOn := cm(tinySettings, map[string]string{"egressProxy": "true"})
	allow := cm(egressAllowCM, map[string]string{"hosts": "# c\napi.anthropic.com\n\n.github.com\n"})
	tests := []struct {
		name   string
		crd    *apiextv1.CustomResourceDefinition
		kube   []runtime.Object
		failed bool
		want   []string
	}{
		{"missing CRD", nil, []runtime.Object{nsObj, saObj, secret(map[string]string{"ANTHROPIC_API_KEY": apiKey})}, true,
			[]string{"✗ CRD", "tiny init -n " + testNS}},
		{"no mcpServers", crdWith(false), []runtime.Object{nsObj, saObj, secret(map[string]string{"ANTHROPIC_API_KEY": apiKey})}, true,
			[]string{"✗", "older than v0.11.0"}},
		{"missing SA", crdWith(true), []runtime.Object{nsObj, secret(map[string]string{"ANTHROPIC_API_KEY": apiKey})}, true,
			[]string{"✗ service account tiny-session"}},
		{"missing namespace", crdWith(true), nil, true, []string{"✗ namespace " + testNS}},
		{"missing secret", crdWith(true), []runtime.Object{nsObj, saObj}, true, []string{"✗ secret tiny-agent-env"}},
		{"empty secret", crdWith(true), []runtime.Object{nsObj, saObj, secret(map[string]string{"OTHER": "x"})}, true,
			[]string{"✗ secret tiny-agent-env has neither"}},
		{"oauth only", crdWith(true), []runtime.Object{nsObj, saObj, secret(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": apiKey}), proxyOn, allow}, false,
			[]string{"! secret tiny-agent-env has only CLAUDE_CODE_OAUTH_TOKEN", "metered ANTHROPIC_API_KEY"}},
		{"proxy off", crdWith(true), []runtime.Object{nsObj, saObj, secret(map[string]string{"ANTHROPIC_API_KEY": apiKey})}, false,
			[]string{"! egress proxy is off", "any HTTPS host"}},
		{"all good", crdWith(true), []runtime.Object{nsObj, saObj, secret(map[string]string{"ANTHROPIC_API_KEY": apiKey}), proxyOn, allow}, false,
			[]string{"✓ CRD", "✓ namespace", "✓ service account", "✓ secret tiny-agent-env has ANTHROPIC_API_KEY",
				"allowed hosts: api.anthropic.com, .github.com", "! network policies are only enforced if your CNI enforces them"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ext *extfake.Clientset
			if tc.crd != nil {
				ext = extfake.NewSimpleClientset(tc.crd)
			} else {
				ext = extfake.NewSimpleClientset()
			}
			var out strings.Builder
			failed := preflight(context.Background(), kubefake.NewSimpleClientset(tc.kube...), ext, testNS, &out)
			if failed != tc.failed {
				t.Errorf("failed = %v, want %v\n%s", failed, tc.failed, out.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output missing %q:\n%s", w, out.String())
				}
			}
			if strings.Contains(out.String(), apiKey) || strings.Contains(out.String(), "SECRETVALUE") {
				t.Errorf("secret value leaked:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "network policies are only enforced") {
				t.Error("CNI warning missing")
			}
		})
	}
}

func TestRenderManifest(t *testing.T) {
	objs, err := renderManifest([]byte(directorForTest()), "bp-x", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 8 {
		t.Fatalf("objects = %d", len(objs))
	}
	// Guard rails go before the Deployment, so a run pod is never admitted without them.
	idx := map[string]int{}
	for i, o := range objs {
		idx[o.GetKind()] = i
	}
	for _, k := range []string{"LimitRange", "ResourceQuota"} {
		if i, ok := idx[k]; !ok || i > idx["Deployment"] {
			t.Errorf("%s missing or after the Deployment: %v", k, idx)
		}
	}
	var out strings.Builder
	if err := printManifest(&out, objs); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, w := range []string{"ghcr.io/boarding-party/director:v1.2.3", "--worker-image=ghcr.io/boarding-party/worker:v1.2.3", "namespace: bp-x",
		"name: BP_ADMIN_TOKEN", "name: bp-director-admin"} {
		if !strings.Contains(s, w) {
			t.Errorf("rendered YAML missing %q", w)
		}
	}
	for _, w := range []string{"kind: LimitRange", "name: bp-defaults", "kind: ResourceQuota", "name: bp-quota",
		"limits.memory: 12Gi", `pods: "12"`, "app.kubernetes.io/name: bp-director"} {
		if !strings.Contains(s, w) {
			t.Errorf("rendered YAML missing %q", w)
		}
	}
	if strings.Contains(s, "kind: Secret") {
		t.Error("the manifest must not carry the admin Secret (it would rotate on every apply)")
	}
	if strings.Contains(s, ":main") {
		t.Error("untagged :main left behind")
	}
	for _, o := range objs {
		if o.GetKind() == "RoleBinding" {
			subs, _, _ := unstructured.NestedSlice(o.Object, "subjects")
			if len(subs) == 0 || subs[0].(map[string]any)["namespace"] != "bp-x" {
				t.Errorf("RoleBinding subjects = %v", subs)
			}
		}
		if o.GetNamespace() != "bp-x" {
			t.Errorf("%s/%s namespace = %q", o.GetKind(), o.GetName(), o.GetNamespace())
		}
	}
}

func goodDeps(applied *[]string, verifyErr error) setupDeps {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: directorDeploy, Namespace: testNS, Generation: 1},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}}}
	return setupDeps{
		kube: kubefake.NewSimpleClientset(nsObj, saObj, dep, secret(map[string]string{"ANTHROPIC_API_KEY": "x"})),
		ext:  extfake.NewSimpleClientset(crdWith(true)),
		apply: func(_ context.Context, o *unstructured.Unstructured) error {
			*applied = append(*applied, o.GetKind()+"/"+o.GetName())
			return nil
		},
		verify: func(context.Context) error { return verifyErr },
	}
}

func TestSetupDryRunAppliesNothing(t *testing.T) {
	var applied []string
	var out strings.Builder
	err := runSetup(context.Background(), goodDeps(&applied, nil), setupOpts{ns: testNS, tag: "main", dryRun: true, timeout: time.Second}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Errorf("applied %v", applied)
	}
	if !strings.Contains(out.String(), "kind: Deployment") || strings.Contains(out.String(), "director ready") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

func TestSetupFailedPreflightAppliesNothing(t *testing.T) {
	var applied []string
	d := goodDeps(&applied, nil)
	d.ext = extfake.NewSimpleClientset()
	err := runSetup(context.Background(), d, setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &strings.Builder{})
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 1 || len(applied) != 0 {
		t.Errorf("err = %v, applied = %v", err, applied)
	}
}

func TestSetupApplies(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	var out strings.Builder
	if err := runSetup(context.Background(), goodDeps(&applied, nil), setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &out); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 8 || applied[0] != "LimitRange/bp-defaults" || applied[1] != "ResourceQuota/bp-quota" {
		t.Errorf("applied = %v", applied)
	}
	for _, w := range []string{"✓ Deployment/bp-director", "✓ director ready", "boardingparty board -n " + testNS} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output missing %q:\n%s", w, out.String())
		}
	}
}

func TestSetupVerifyTimeout(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	err := runSetup(context.Background(), goodDeps(&applied, errors.New("boom")), setupOpts{ns: testNS, tag: "main", timeout: 50 * time.Millisecond}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

func directorForTest() string { return string(deploy.Director) }

func directorAt(tag string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: directorDeploy, Namespace: testNS, Generation: 1},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "director", Image: directorImage + ":" + tag}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}}}
}

func ensureDeps(applied *[]string, withTiny bool, objs ...runtime.Object) setupDeps {
	if withTiny {
		objs = append(objs, nsObj, saObj, secret(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	}
	d := goodDeps(applied, nil)
	d.kube = kubefake.NewSimpleClientset(objs...)
	if !withTiny {
		d.ext = extfake.NewSimpleClientset()
	}
	return d
}

func TestEnsureDirectorMissingInstalls(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	d := ensureDeps(&applied, true)
	// The fake never creates the Deployment on apply, so add it once applied.
	d.apply = func(_ context.Context, o *unstructured.Unstructured) error {
		applied = append(applied, o.GetKind())
		if o.GetKind() == "Deployment" {
			_, err := d.kube.AppsV1().Deployments(testNS).Create(context.Background(), directorAt("1.0.0"), metav1.CreateOptions{})
			return err
		}
		return nil
	}
	var out strings.Builder
	if _, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &out); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 8 || !strings.Contains(out.String(), "installing the director") || !strings.Contains(out.String(), "! network policies") {
		t.Errorf("applied=%v out=%s", applied, out.String())
	}
}

func TestEnsureDirectorTinyMissing(t *testing.T) {
	var applied []string
	_, err := ensureDirector(context.Background(), ensureDeps(&applied, false), setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "run `tiny init -n "+testNS+"`") || !strings.Contains(err.Error(), "✗ CRD") {
		t.Errorf("err = %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied %v", applied)
	}
}

func TestEnsureDirectorUpgrade(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	var out strings.Builder
	d := ensureDeps(&applied, true, directorAt("0.9.0"))
	if _, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &out); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 8 || !strings.Contains(out.String(), "upgrading the director in "+testNS+" (0.9.0 -> 1.0.0)") {
		t.Errorf("applied=%v out=%s", applied, out.String())
	}
}

func TestEnsureDirectorCurrentIsSilent(t *testing.T) {
	var applied []string
	var out strings.Builder
	// tiny is missing too: the fast path must not even look.
	d := ensureDeps(&applied, false, directorAt("1.0.0"), adminSecretObj("tok-1"))
	verified := 0
	d.verify = func(context.Context) error { verified++; return nil }
	tok, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 || out.Len() != 0 || verified != 0 {
		t.Errorf("applied=%v verified=%d out=%q", applied, verified, out.String())
	}
	if tok != "tok-1" {
		t.Errorf("token = %q", tok)
	}
}

func adminSecretObj(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminSecret, Namespace: testNS, UID: "uid-1"},
		Data:       map[string][]byte{adminSecretKey: []byte(token)},
	}
}

func TestEnsureDirectorCurrentButAdminSecretMissingReinstalls(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	var out strings.Builder
	d := ensureDeps(&applied, true, directorAt("1.0.0"))
	var order []string
	apply := d.apply
	d.apply = func(ctx context.Context, o *unstructured.Unstructured) error {
		if _, ok := adminSecretValue(t, d.kube); !ok {
			t.Errorf("%s applied before the secret existed", o.GetKind())
		}
		order = append(order, "apply")
		return apply(ctx, o)
	}
	d.verify = func(context.Context) error { order = append(order, "verify"); return nil }
	tok, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &out)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := adminSecretValue(t, d.kube)
	if !ok || len(want) != 64 || tok != want {
		t.Errorf("secret %q ok=%v, token %q", want, ok, tok)
	}
	if len(applied) != 8 {
		t.Errorf("applied = %v", applied)
	}
	if len(order) == 0 || order[len(order)-1] != "verify" {
		t.Errorf("verify didn't run after the apply: %v", order)
	}
	if !strings.Contains(out.String(), "reinstalling the director in "+testNS+" (its admin key is missing)") {
		t.Errorf("out = %s", out.String())
	}
	if strings.Contains(out.String(), tok) {
		t.Errorf("token printed")
	}
}

func TestInstallStampsSecretUIDOnPodTemplate(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	d := ensureDeps(&applied, true, directorAt("0.9.0"), adminSecretObj("tok-1"))
	var uid string
	d.apply = func(_ context.Context, o *unstructured.Unstructured) error {
		if o.GetKind() == "Deployment" {
			uid, _, _ = unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", adminSecretAnnotation)
		}
		return nil
	}
	tok, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if uid != "uid-1" || tok != "tok-1" {
		t.Errorf("annotation = %q, token = %q", uid, tok)
	}
	if v, _ := adminSecretValue(t, d.kube); v != "tok-1" {
		t.Errorf("existing secret changed: %q", v)
	}
}

func TestTagOf(t *testing.T) {
	for in, want := range map[string]string{
		"registry:5000/ns/director":              "",
		"registry:5000/ns/director:1.2.3":        "1.2.3",
		"ghcr.io/boarding-party/director:main":   "main",
		"ghcr.io/x/director@sha256:abcdef":       "",
		"ghcr.io/x/director:1.2.3@sha256:abcdef": "1.2.3",
		"director":                               "",
	} {
		if got := tagOf(in); got != want {
			t.Errorf("tagOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRolloutComplete(t *testing.T) {
	dep := directorAt("1.0.0")
	dep.Generation = 2 // spec updated, controller hasn't caught up; Available is still True
	if rolloutComplete(dep) {
		t.Error("stale generation counted as complete")
	}
	dep.Status.ObservedGeneration = 2
	dep.Status.UpdatedReplicas = 0
	if rolloutComplete(dep) {
		t.Error("not updated counted as complete")
	}
	dep.Status.UpdatedReplicas = 1
	if !rolloutComplete(dep) {
		t.Error("complete rollout not recognised")
	}
}

func TestEnsureWaitsForRollout(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	stale := directorAt("0.9.0")
	stale.Generation = 2 // Available=True from the old pod, but not observed
	d := ensureDeps(&applied, true, stale)
	go func() {
		time.Sleep(30 * time.Millisecond)
		fresh := directorAt("1.0.0")
		fresh.Generation, fresh.Status.ObservedGeneration = 2, 2
		d.kube.AppsV1().Deployments(testNS).Update(context.Background(), fresh, metav1.UpdateOptions{})
	}()
	start := time.Now()
	if _, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: 2 * time.Second}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Error("wait returned before the rollout completed")
	}
}

func TestEnsureVerifies(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	d := ensureDeps(&applied, true, directorAt("0.9.0"))
	d.verify = func(context.Context) error { return errors.New("not yet") }
	_, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: 50 * time.Millisecond}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "not yet") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitErrorIsReported(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	d := ensureDeps(&applied, true)
	d.kube.(*kubefake.Clientset).PrependReactor("get", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		if len(applied) > 0 {
			return true, nil, errors.New("apiserver on fire")
		}
		return false, nil, nil
	})
	_, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: 50 * time.Millisecond}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "apiserver on fire") {
		t.Errorf("err = %v", err)
	}
}

func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "x", errors.New("nope"))
}

func TestPreflightForbiddenClusterReadsWarn(t *testing.T) {
	kube := kubefake.NewSimpleClientset(saObj, secret(map[string]string{"ANTHROPIC_API_KEY": "x"}))
	kube.PrependReactor("get", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden("namespaces") })
	ext := extfake.NewSimpleClientset()
	ext.PrependReactor("get", "customresourcedefinitions", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden("customresourcedefinitions")
	})
	var out strings.Builder
	if preflight(context.Background(), kube, ext, testNS, &out) {
		t.Errorf("should continue:\n%s", out.String())
	}
	for _, w := range []string{"! CRD", "no permission (get customresourcedefinitions)", "! namespace", "✓ service account"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("missing %q:\n%s", w, out.String())
		}
	}
}

func TestEnsureForbiddenNamespacedIsNotTinyMissing(t *testing.T) {
	var applied []string
	d := ensureDeps(&applied, true)
	d.kube.(*kubefake.Clientset).PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden("secrets")
	})
	_, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &strings.Builder{})
	if err == nil || strings.Contains(err.Error(), "tiny init") || !strings.Contains(err.Error(), "no permission (get secrets)") {
		t.Errorf("err = %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied %v", applied)
	}
}

func TestConflictHint(t *testing.T) {
	var applied []string
	d := ensureDeps(&applied, true)
	d.apply = func(context.Context, *unstructured.Unstructured) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "bp-director", fmt.Errorf("conflict"))
	}
	_, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "1.0.0", timeout: time.Second}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "boardingparty setup --force-conflicts") {
		t.Errorf("err = %v", err)
	}
}

func TestTagFromBuild(t *testing.T) {
	cases := []struct {
		name, version, rev string
		modified           bool
		tag, note          string
	}{
		{"release", "v1.2.3", "abcdef1234567", false, "1.2.3", ""},
		{"release ignores dirty", "v1.2.3", "abcdef1234567", true, "1.2.3", ""},
		{"clean dev", "(devel)", "abcdef1234567890", false, "sha-abcdef1", ""},
		{"modified dev", "(devel)", "abcdef1234567890", true, "main", devBuildNote},
		{"no revision", "(devel)", "", false, "main", devBuildNote},
	}
	for _, c := range cases {
		tag, note := tagFromBuild(c.version, c.rev, c.modified)
		if tag != c.tag || note != c.note {
			t.Errorf("%s: got %q %q, want %q %q", c.name, tag, note, c.tag, c.note)
		}
	}
}

func TestEnsureDirectorUnpublishedImageAppliesNothing(t *testing.T) {
	var applied []string
	d := ensureDeps(&applied, true)
	d.imageExists = func(context.Context, string, string) (bool, error) { return false, nil }
	_, err := ensureDirector(context.Background(), d, setupOpts{ns: testNS, tag: "sha-abcdef1", timeout: time.Second}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "images for abcdef1 aren't published yet") {
		t.Fatalf("err = %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v", applied)
	}
}

func TestCheckImagesSkipsNonSha(t *testing.T) {
	d := setupDeps{imageExists: func(context.Context, string, string) (bool, error) { return false, nil }}
	if err := checkImages(context.Background(), d, "main"); err != nil {
		t.Error(err)
	}
	if err := checkImages(context.Background(), d, "sha-abcdef1"); err == nil {
		t.Error("want error")
	}
}
