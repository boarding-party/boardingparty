package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	extfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

func adminSecretValue(t *testing.T, kube kubernetes.Interface) (string, bool) {
	t.Helper()
	s, err := kube.CoreV1().Secrets(testNS).Get(context.Background(), adminSecret, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	return string(s.Data[adminSecretKey]), true
}

func TestSetupCreatesAdminSecretBeforeTheDeploymentAndOnlyOnce(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	d := goodDeps(&applied, nil)
	apply := d.apply
	d.apply = func(ctx context.Context, o *unstructured.Unstructured) error {
		if _, ok := adminSecretValue(t, d.kube); !ok {
			t.Errorf("%s/%s applied before the admin secret existed", o.GetKind(), o.GetName())
		}
		return apply(ctx, o)
	}
	var out strings.Builder
	if err := runSetup(context.Background(), d, setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &out); err != nil {
		t.Fatal(err)
	}
	tok, ok := adminSecretValue(t, d.kube)
	if !ok || len(tok) != 64 || strings.Trim(tok, "0123456789abcdef") != "" {
		t.Fatalf("admin token = %q", tok)
	}
	s, _ := d.kube.CoreV1().Secrets(testNS).Get(context.Background(), adminSecret, metav1.GetOptions{})
	if s.Labels[managedByLabel] != managedByValue || s.Labels["app.kubernetes.io/name"] != directorDeploy {
		t.Errorf("labels = %v (leave wouldn't find it)", s.Labels)
	}
	if strings.Contains(out.String(), tok) {
		t.Errorf("token printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Secret/"+adminSecret) {
		t.Errorf("creation not reported:\n%s", out.String())
	}
	// A second setup keeps the same secret.
	out.Reset()
	if err := runSetup(context.Background(), d, setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &out); err != nil {
		t.Fatal(err)
	}
	if again, _ := adminSecretValue(t, d.kube); again != tok {
		t.Errorf("secret rotated")
	}
	if strings.Contains(out.String(), "Secret/"+adminSecret) {
		t.Errorf("reported creating it again:\n%s", out.String())
	}
}

func TestSetupNeverOverwritesAnExistingAdminSecret(t *testing.T) {
	waitPoll = time.Millisecond
	var applied []string
	d := goodDeps(&applied, nil)
	_, err := d.kube.CoreV1().Secrets(testNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminSecret, Namespace: testNS},
		Data:       map[string][]byte{adminSecretKey: []byte("keep-me")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runSetup(context.Background(), d, setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if v, _ := adminSecretValue(t, d.kube); v != "keep-me" {
		t.Errorf("secret overwritten: %q", v)
	}
}

func TestSetupDryRunAndFailedPreflightCreateNoSecret(t *testing.T) {
	var applied []string
	d := goodDeps(&applied, nil)
	if err := runSetup(context.Background(), d, setupOpts{ns: testNS, tag: "main", dryRun: true, timeout: time.Second}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := adminSecretValue(t, d.kube); ok {
		t.Error("dry run created the secret")
	}
	d.ext = extfake.NewSimpleClientset()
	_ = runSetup(context.Background(), d, setupOpts{ns: testNS, tag: "main", timeout: time.Second}, &strings.Builder{})
	if _, ok := adminSecretValue(t, d.kube); ok {
		t.Error("failed preflight created the secret")
	}
}

func TestReadAdminToken(t *testing.T) {
	var applied []string
	kube := goodDeps(&applied, nil).kube
	if _, err := readAdminToken(context.Background(), kube, testNS); err == nil || !strings.Contains(err.Error(), "boardingparty setup") {
		t.Errorf("missing secret err = %v", err)
	}
	if _, err := ensureAdminSecret(context.Background(), kube, testNS); err != nil {
		t.Fatal(err)
	}
	tok, err := readAdminToken(context.Background(), kube, testNS)
	if want, _ := adminSecretValue(t, kube); err != nil || tok != want {
		t.Errorf("token = %q, %v", tok, err)
	}
}

func TestClientSendsBearer(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "s3cret")
	if _, err := c.Runs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.PutProject(context.Background(), []byte("name: x")); err != nil {
		t.Fatal(err)
	}
	for _, h := range got {
		if h != "Bearer s3cret" {
			t.Errorf("Authorization = %q", h)
		}
	}
	if len(got) != 2 {
		t.Errorf("requests = %d", len(got))
	}
}
