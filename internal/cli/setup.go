package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/boarding-party/boardingparty/deploy"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
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
	// notSetUp is set when a check proved tiny's setup is absent or too old,
	// as opposed to a check we weren't allowed to make.
	notSetUp bool
}

func (c *checker) ok(format string, a ...any) {
	fmt.Fprintf(c.out, "✓ %s\n", clean(fmt.Sprintf(format, a...)))
}
func (c *checker) warn(format string, a ...any) {
	fmt.Fprintf(c.out, "! %s\n", clean(fmt.Sprintf(format, a...)))
}
func (c *checker) missing(format string, a ...any) {
	c.notSetUp = true
	c.fail(format, a...)
}
func (c *checker) fail(format string, a ...any) {
	c.failed = true
	fmt.Fprintf(c.out, "✗ %s\n", clean(fmt.Sprintf(format, a...)))
}

// preflight runs the read-only checks and reports whether any failed.
// It never prints Secret values, only key names.
func preflight(ctx context.Context, kube kubernetes.Interface, ext apiextclient.Interface, ns string, out io.Writer) (failed bool) {
	return preflightCheck(ctx, kube, ext, ns, out).failed
}

// denied reports a read the user's credentials aren't allowed to make.
func denied(verb, resource string) string {
	return fmt.Sprintf("couldn't check: no permission (%s %s)", verb, resource)
}

func preflightCheck(ctx context.Context, kube kubernetes.Interface, ext apiextclient.Interface, ns string, out io.Writer) *checker {
	c := &checker{out: out}
	tinyInit := fmt.Sprintf("run `tiny init -n %s`", ns)

	crd, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, sessionCRD, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		c.missing("CRD %s not found: tiny isn't installed, %s", sessionCRD, tinyInit)
	case apierrors.IsForbidden(err):
		c.warn("CRD %s: %s; skipped, the check needs cluster-scoped read access", sessionCRD, denied("get", "customresourcedefinitions"))
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
			c.missing("CRD %s lacks spec.mcpServers: tiny is older than v0.11.0, upgrade tiny", sessionCRD)
		}
	}

	_, err = kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	nsOK := err == nil || apierrors.IsForbidden(err)
	switch {
	case apierrors.IsNotFound(err):
		c.missing("namespace %s not found: %s", ns, tinyInit)
	case apierrors.IsForbidden(err):
		c.warn("namespace %s: %s; skipped, the check needs cluster-scoped read access", ns, denied("get", "namespaces"))
	case err != nil:
		c.fail("namespace %s: %v", ns, err)
	default:
		c.ok("namespace %s", ns)
	}
	if nsOK {
		_, err = kube.CoreV1().ServiceAccounts(ns).Get(ctx, sessionSA, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			c.missing("service account %s not found in %s: %s", sessionSA, ns, tinyInit)
		case apierrors.IsForbidden(err):
			c.fail("service account %s: %s", sessionSA, denied("get", "serviceaccounts"))
		case err != nil:
			c.fail("service account %s: %v", sessionSA, err)
		default:
			c.ok("service account %s", sessionSA)
		}
	}

	checkAgentSecret(ctx, kube, ns, c)
	checkEgress(ctx, kube, ns, c)
	c.warn("network policies are only enforced if your CNI enforces them")
	return c
}

func checkAgentSecret(ctx context.Context, kube kubernetes.Interface, ns string, c *checker) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, agentSecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		c.missing("secret %s not found in %s: add your agent credentials to tiny's agent secret", agentSecret, ns)
		return
	case apierrors.IsForbidden(err):
		c.fail("secret %s: %s", agentSecret, denied("get", "secrets"))
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
		c.missing("secret %s has neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN", agentSecret)
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
	for _, doc := range strings.Split(text, "\n---\n") {
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
	force   bool
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
	wctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	if err := installDirector(wctx, d, o, objs, out); err != nil {
		return err
	}
	if err := waitVerify(wctx, d); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ director ready\n\nNext:\n  boardingparty apply -f project.yaml -n %s\n  boardingparty run \"<persona>\" \"<story>\" --wait -n %s\n", o.ns, o.ns)
	return nil
}

// installDirector applies the objects one by one and waits for the rollout.
func installDirector(ctx context.Context, d setupDeps, o setupOpts, objs []*unstructured.Unstructured, out io.Writer) error {
	for _, obj := range objs {
		if err := d.apply(ctx, obj); err != nil {
			if apierrors.IsConflict(err) && !o.force {
				return fmt.Errorf("apply %s/%s: %w\nthe director was previously applied by another tool; run `boardingparty setup --force-conflicts` once", obj.GetKind(), obj.GetName(), err)
			}
			return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		fmt.Fprintf(out, "✓ %s/%s\n", clean(obj.GetKind()), clean(obj.GetName()))
	}
	var lastErr error
	err := wait.PollUntilContextCancel(ctx, waitPoll, true, func(ctx context.Context) (bool, error) {
		dep, err := d.kube.AppsV1().Deployments(o.ns).Get(ctx, directorDeploy, metav1.GetOptions{})
		if err != nil {
			lastErr = err
			return false, nil
		}
		lastErr = nil
		return rolloutComplete(dep), nil
	})
	if err != nil {
		return fmt.Errorf("deployment %s not rolled out within %s: %v (last error: %v)", directorDeploy, o.timeout, err, lastErr)
	}
	fmt.Fprintf(out, "✓ deployment/%s rolled out\n", directorDeploy)
	return nil
}

// waitVerify retries the admin-API check until it passes or ctx ends.
func waitVerify(ctx context.Context, d setupDeps) error {
	var verr error
	err := wait.PollUntilContextCancel(ctx, waitPoll, true, func(ctx context.Context) (bool, error) {
		verr = d.verify(ctx)
		return verr == nil, nil
	})
	if err != nil {
		return fmt.Errorf("director not answering: %v (last error: %v)", err, verr)
	}
	return nil
}

// ensureDirector is what every command that needs the director runs first.
// Fast path: one Deployment GET; if the director is there at the wanted tag,
// nothing else happens. Otherwise tiny's prerequisites are checked (read-only,
// failing with a pointer to `tiny init`), the warnings are printed once, and
// the director is installed or upgraded in place.
func ensureDirector(ctx context.Context, d setupDeps, o setupOpts, out io.Writer) error {
	dep, err := d.kube.AppsV1().Deployments(o.ns).Get(ctx, directorDeploy, metav1.GetOptions{})
	if err == nil && imageTag(dep) == o.tag {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get deployment %s: %w", directorDeploy, err)
	}
	upgrade := err == nil
	var buf strings.Builder
	if pc := preflightCheck(ctx, d.kube, d.ext, o.ns, &buf); pc.failed {
		var failed []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.HasPrefix(l, "✗") {
				failed = append(failed, l)
			}
		}
		if !pc.notSetUp {
			return fmt.Errorf("couldn't check tiny's setup in namespace %s: your credentials lack a permission\n%s",
				o.ns, strings.Join(failed, "\n"))
		}
		return fmt.Errorf("tiny isn't set up in namespace %s: run `tiny init -n %s` (and `tiny setup` for the agent key)\n%s",
			o.ns, o.ns, strings.Join(failed, "\n"))
	}
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "!") {
			fmt.Fprintln(out, l)
		}
	}
	objs, err := renderManifest(deploy.Director, o.ns, o.tag)
	if err != nil {
		return err
	}
	if !upgrade {
		fmt.Fprintf(out, "installing the director in %s (image tag %s)\n", o.ns, o.tag)
	} else {
		fmt.Fprintf(out, "upgrading the director in %s (%s -> %s)\n", o.ns, imageTag(dep), o.tag)
	}
	wctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	if err := installDirector(wctx, d, o, objs, out); err != nil {
		return err
	}
	return waitVerify(wctx, d)
}

// imageTag returns the tag of the director container image, or "" when the
// image has none (untagged or digest-only).
func imageTag(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "director" {
			return tagOf(c.Image)
		}
	}
	return ""
}

func tagOf(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	last := image[strings.LastIndex(image, "/")+1:] // a registry port sits before the last slash
	if j := strings.LastIndex(last, ":"); j >= 0 {
		return last[j+1:]
	}
	return ""
}

var releaseSemver = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// wantedTag is the CLI's own release version without the leading v, else main.
func wantedTag() string {
	if bi, ok := debug.ReadBuildInfo(); ok && releaseSemver.MatchString(bi.Main.Version) {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return defaultImageTag
}

// rolloutComplete is true once the controller has seen the latest spec and
// every replica is updated and available. The Available condition alone is not
// enough: it stays true on the old pod right after an apply.
func rolloutComplete(dep *appsv1.Deployment) bool {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	st := dep.Status
	return st.ObservedGeneration >= dep.Generation && st.UpdatedReplicas == want &&
		st.AvailableReplicas == want && st.UnavailableReplicas == 0
}

// serverSideApplier applies objects with server-side apply, conflicts forced only when asked.
func serverSideApplier(cfg *rest.Config, force bool) (func(context.Context, *unstructured.Unstructured) error, error) {
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
		Use:   "setup",
		Short: "Check the cluster and install the director",
		Long: "Checks that tiny is ready in the namespace, then installs (or upgrades in place)\n" +
			"the boardingparty director with server-side apply and waits until it answers.\n" +
			"Prerequisite: `tiny init -n <namespace>` and tiny's agent secret.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.chooseTarget(cmd.Context()); err != nil {
				return err
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
			if d.apply, err = serverSideApplier(cfg, o.force); err != nil {
				return err
			}
			d.verify = verifyDirector(cfg, o.ns)
			return runSetup(cmd.Context(), d, o, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&o.tag, "image-tag", wantedTag(), "director and worker image tag")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "run the checks and print the rendered manifest; apply nothing")
	cmd.Flags().BoolVar(&o.force, "force-conflicts", false, "take over fields owned by another tool (use once after a kubectl apply)")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 3*time.Minute, "how long to wait for the director")
	return cmd
}
