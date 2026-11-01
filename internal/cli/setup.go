package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/boarding-party/boardingparty/deploy"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

const (
	fieldManager    = "boardingparty"
	sessionCRD      = "sessions.agents.tinysystems.io"
	sessionSA       = "tiny-session"
	agentSecret     = "tiny-agent-env"
	tinySettings    = "tiny-settings"
	egressAllowCM   = "tiny-egress-allow"
	egressAllowKey  = "hosts"
	directorDeploy  = "bp-director"
	directorImage   = "ghcr.io/boarding-party/director"
	workerImage     = "ghcr.io/boarding-party/worker"
	defaultImageTag = "main"
)

// checker prints one line per preflight check and remembers failures.
type checker struct {
	out    io.Writer
	failed bool
}

func (c *checker) ok(format string, a ...any) {
	fmt.Fprintf(c.out, "✓ %s\n", clean(fmt.Sprintf(format, a...)))
}
func (c *checker) warn(format string, a ...any) {
	fmt.Fprintf(c.out, "! %s\n", clean(fmt.Sprintf(format, a...)))
}
func (c *checker) fail(format string, a ...any) {
	c.failed = true
	fmt.Fprintf(c.out, "✗ %s\n", clean(fmt.Sprintf(format, a...)))
}

// preflight runs the read-only checks and reports whether any failed.
// It never prints Secret values, only key names.
func preflight(ctx context.Context, kube kubernetes.Interface, ext apiextclient.Interface, ns string, out io.Writer) (failed bool) {
	c := &checker{out: out}
	tinyInit := fmt.Sprintf("run `tiny init -n %s`", ns)

	crd, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, sessionCRD, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		c.fail("CRD %s not found: tiny isn't installed, %s", sessionCRD, tinyInit)
	case err != nil:
		c.fail("CRD %s: %v", sessionCRD, err)
	default:
		has := false
		for _, v := range crd.Spec.Versions {
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				continue
			}
			if spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]; ok {
				if _, ok := spec.Properties["mcpServers"]; ok {
					has = true
				}
			}
		}
		if has {
			c.ok("CRD %s has spec.mcpServers", sessionCRD)
		} else {
			c.fail("CRD %s lacks spec.mcpServers: tiny is older than v0.11.0, upgrade tiny", sessionCRD)
		}
	}

	_, err = kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	nsOK := err == nil
	switch {
	case apierrors.IsNotFound(err):
		c.fail("namespace %s not found: %s", ns, tinyInit)
	case err != nil:
		c.fail("namespace %s: %v", ns, err)
	default:
		c.ok("namespace %s", ns)
	}
	if nsOK {
		_, err = kube.CoreV1().ServiceAccounts(ns).Get(ctx, sessionSA, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			c.fail("service account %s not found in %s: %s", sessionSA, ns, tinyInit)
		case err != nil:
			c.fail("service account %s: %v", sessionSA, err)
		default:
			c.ok("service account %s", sessionSA)
		}
	}

	checkAgentSecret(ctx, kube, ns, c)
	checkEgress(ctx, kube, ns, c)
	c.warn("network policies are only enforced if your CNI enforces them")
	return c.failed
}

func checkAgentSecret(ctx context.Context, kube kubernetes.Interface, ns string, c *checker) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, agentSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		c.fail("secret %s not found in %s: add your agent credentials to tiny's agent secret", agentSecret, ns)
		return
	case err != nil:
		c.fail("secret %s: %v", agentSecret, err)
		return
	}
	_, key := sec.Data["ANTHROPIC_API_KEY"]
	_, oauth := sec.Data["CLAUDE_CODE_OAUTH_TOKEN"]
	switch {
	case key:
		c.ok("secret %s has ANTHROPIC_API_KEY", agentSecret)
	case oauth:
		c.warn("secret %s has only CLAUDE_CODE_OAUTH_TOKEN: a subscription token; automated runs should use a metered ANTHROPIC_API_KEY under Anthropic's terms", agentSecret)
	default:
		c.fail("secret %s has neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN", agentSecret)
	}
}

func checkEgress(ctx context.Context, kube kubernetes.Interface, ns string, c *checker) {
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, tinySettings, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.warn("configmap %s: %v", tinySettings, err)
		return
	}
	if err != nil || cm.Data["egressProxy"] != "true" {
		c.warn("egress proxy is off: no hostname allow-list, agents can reach any HTTPS host")
		return
	}
	al, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, egressAllowCM, metav1.GetOptions{})
	if err != nil {
		c.warn("egress proxy is on but allow-list %s can't be read: %v", egressAllowCM, err)
		return
	}
	var hosts []string
	sc := bufio.NewScanner(strings.NewReader(al.Data[egressAllowKey]))
	for sc.Scan() {
		if h := strings.TrimSpace(sc.Text()); h != "" && !strings.HasPrefix(h, "#") {
			hosts = append(hosts, h)
		}
	}
	c.ok("egress proxy on; allowed hosts: %s", strings.Join(hosts, ", "))
}

// renderManifest returns the director objects for ns with the image tag applied.
func renderManifest(raw []byte, ns, tag string) ([]*unstructured.Unstructured, error) {
	text := strings.ReplaceAll(string(raw), directorImage+":main", directorImage+":"+tag)
	text = strings.ReplaceAll(text, workerImage+":main", workerImage+":"+tag)
	var objs []*unstructured.Unstructured
	for _, doc := range strings.Split(text, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		u.SetNamespace(ns)
		if u.GetKind() == "RoleBinding" {
			subs, _, _ := unstructured.NestedSlice(u.Object, "subjects")
			for _, s := range subs {
				if m, ok := s.(map[string]any); ok && m["kind"] == "ServiceAccount" {
					m["namespace"] = ns
				}
			}
			_ = unstructured.SetNestedSlice(u.Object, subs, "subjects")
		}
		objs = append(objs, u)
	}
	return objs, nil
}

func printManifest(out io.Writer, objs []*unstructured.Unstructured) error {
	for i, o := range objs {
		b, err := yaml.Marshal(o.Object)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Fprintln(out, "---")
		}
		out.Write(b)
	}
	return nil
}

type setupOpts struct {
	ns, tag string
	dryRun  bool
	timeout time.Duration
}

// setupDeps are the cluster-facing pieces, replaceable in tests.
type setupDeps struct {
	kube  kubernetes.Interface
	ext   apiextclient.Interface
	apply func(ctx context.Context, obj *unstructured.Unstructured) error
	// verify checks the director answers GET /api/runs.
	verify func(ctx context.Context) error
}

var waitPoll = 2 * time.Second

func runSetup(ctx context.Context, d setupDeps, o setupOpts, out io.Writer) error {
	if preflight(ctx, d.kube, d.ext, o.ns, out) {
		return ExitError{Code: 1}
	}
	objs, err := renderManifest(deploy.Director, o.ns, o.tag)
	if err != nil {
		return err
	}
	if o.dryRun {
		fmt.Fprintln(out, "---")
		return printManifest(out, objs)
	}
	for _, obj := range objs {
		if err := d.apply(ctx, obj); err != nil {
			return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		fmt.Fprintf(out, "✓ %s/%s\n", clean(obj.GetKind()), clean(obj.GetName()))
	}
	wctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	err = wait.PollUntilContextCancel(wctx, waitPoll, true, func(ctx context.Context) (bool, error) {
		dep, err := d.kube.AppsV1().Deployments(o.ns).Get(ctx, directorDeploy, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return deploymentAvailable(dep), nil
	})
	if err != nil {
		return fmt.Errorf("deployment %s not available within %s: %w", directorDeploy, o.timeout, err)
	}
	fmt.Fprintf(out, "✓ deployment/%s available\n", directorDeploy)
	var verr error
	err = wait.PollUntilContextCancel(wctx, waitPoll, true, func(ctx context.Context) (bool, error) {
		verr = d.verify(ctx)
		return verr == nil, nil
	})
	if err != nil {
		return fmt.Errorf("director not answering: %v (last error: %v)", err, verr)
	}
	fmt.Fprintf(out, "✓ director ready\n\nNext:\n  boardingparty apply -f project.yaml -n %s\n  boardingparty run \"<persona>\" \"<story>\" --wait -n %s\n", o.ns, o.ns)
	return nil
}

func deploymentAvailable(dep *appsv1.Deployment) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// serverSideApplier applies objects with server-side apply, conflicts not forced.
func serverSideApplier(cfg *rest.Config) (func(context.Context, *unstructured.Unstructured) error, error) {
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	return func(ctx context.Context, obj *unstructured.Unstructured) error {
		gvk := obj.GroupVersionKind()
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}
		body, err := obj.MarshalJSON()
		if err != nil {
			return err
		}
		force := false
		_, err = dc.Resource(m.Resource).Namespace(obj.GetNamespace()).Patch(ctx, obj.GetName(), types.ApplyPatchType, body,
			metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
		return err
	}, nil
}

// verifyDirector port-forwards to the director and expects GET /api/runs = 200.
func verifyDirector(cfg *rest.Config, ns string) func(context.Context) error {
	return func(ctx context.Context) error {
		base, stop, err := forward(ctx, cfg, ns, directorSelector, adminPort)
		if err != nil {
			return err
		}
		defer stop()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs", nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET /api/runs: %s", clean(resp.Status))
		}
		return nil
	}
}

func newSetup(g *globals) *cobra.Command {
	var o setupOpts
	cmd := &cobra.Command{
		Use:   "setup -n <namespace>",
		Short: "Check the cluster and install the director",
		Long: "Checks that tiny is ready in the namespace, then installs (or upgrades in place)\n" +
			"the boardingparty director with server-side apply and waits until it answers.\n" +
			"Prerequisite: `tiny init -n <namespace>` and tiny's agent secret.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.namespace == "" {
				return errors.New("-n/--namespace is required")
			}
			o.ns = g.namespace
			cfg, _, err := g.kubeConfig()
			if err != nil {
				return err
			}
			d := setupDeps{}
			if d.kube, err = kubernetes.NewForConfig(cfg); err != nil {
				return err
			}
			if d.ext, err = apiextclient.NewForConfig(cfg); err != nil {
				return err
			}
			if d.apply, err = serverSideApplier(cfg); err != nil {
				return err
			}
			d.verify = verifyDirector(cfg, o.ns)
			return runSetup(cmd.Context(), d, o, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&o.tag, "image-tag", defaultImageTag, "director and worker image tag")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "run the checks and print the rendered manifest; apply nothing")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 3*time.Minute, "how long to wait for the director")
	return cmd
}
