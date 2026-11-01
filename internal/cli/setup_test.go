package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boarding-party/boardingparty/deploy"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
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
	if len(objs) != 6 {
		t.Fatalf("objects = %d", len(objs))
	}
	var out strings.Builder
	if err := printManifest(&out, objs); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, w := range []string{"ghcr.io/boarding-party/director:v1.2.3", "--worker-image=ghcr.io/boarding-party/worker:v1.2.3", "namespace: bp-x"} {
		if !strings.Contains(s, w) {
			t.Errorf("rendered YAML missing %q", w)
		}
	}
	if strings.Contains(s, ":main") {
		t.Error("untagged :main left behind")
	}
	for _, o := range objs {
		if o.GetNamespace() != "bp-x" {
			t.Errorf("%s/%s namespace = %q", o.GetKind(), o.GetName(), o.GetNamespace())
		}
	}
}

func goodDeps(applied *[]string, verifyErr error) setupDeps {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: directorDeploy, Namespace: testNS},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}}}
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
	if len(applied) != 6 || applied[0] != "ServiceAccount/bp-director" {
		t.Errorf("applied = %v", applied)
	}
	for _, w := range []string{"✓ Deployment/bp-director", "✓ director ready", "boardingparty apply -f project.yaml -n " + testNS} {
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
