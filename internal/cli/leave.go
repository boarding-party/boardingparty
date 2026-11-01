package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1 "github.com/tiny-systems/tiny/api/v1alpha1"
)

const (
	managedByLabel    = "app.kubernetes.io/managed-by"
	managedByValue    = "boardingparty"
	managedSelector   = managedByLabel + "=" + managedByValue + ",app.kubernetes.io/name=bp-director"
	runSessionLabel   = "boardingparty.io/run"
	sessionPodLabel   = "tinysystems.io/session"
	runPodPrefix      = "bp-run-"
	directorDataClaim = "bp-director-data"
)

// leaveDeps is everything leave touches, so it can be tested with fakes.
type leaveDeps struct {
	kube kubernetes.Interface
	ctrl ctrlclient.Client // knows agentsv1
	poll time.Duration
}

type leaveOpts struct {
	ns          string
	keepData    bool
	yes         bool
	interactive bool
	timeout     time.Duration
	in          io.Reader
}

// leaveObj is one director object to delete.
type leaveObj struct {
	kind, name string
	del        func(ctx context.Context) error
}

// collectObjects finds what boardingparty installed, in deletion order: the
// labelled objects plus, for installs made before the labels existed, the six
// by name. It never selects anything tiny owns.
func collectObjects(ctx context.Context, d leaveDeps, o leaveOpts) ([]leaveObj, error) {
	ns := o.ns
	sel := metav1.ListOptions{LabelSelector: managedSelector}
	var out []leaveObj
	add := func(kind string, list func() ([]string, error), names []string, del func(ctx context.Context, name string) error) error {
		found, err := list()
		if err != nil {
			return fmt.Errorf("list %s: %w", kind, err)
		}
		seen := map[string]bool{}
		for _, n := range append(found, names...) {
			if seen[n] {
				continue
			}
			seen[n] = true
			n := n
			out = append(out, leaveObj{kind: kind, name: n, del: func(ctx context.Context) error { return del(ctx, n) }})
		}
		return nil
	}
	type spec struct {
		kind   string
		legacy string
		list   func() ([]string, error)
		get    func(ctx context.Context, name string) error
		del    func(ctx context.Context, name string) error
	}
	names := func(n int, f func(i int) string) []string {
		r := make([]string, n)
		for i := range r {
			r[i] = f(i)
		}
		return r
	}
	specs := []spec{
		{"deployment", directorDeploy, func() ([]string, error) {
			l, err := d.kube.AppsV1().Deployments(ns).List(ctx, sel)
			if err != nil {
				return nil, err
			}
			return names(len(l.Items), func(i int) string { return l.Items[i].Name }), nil
		}, func(ctx context.Context, n string) error {
			_, err := d.kube.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{})
			return err
		}, func(ctx context.Context, n string) error {
			return d.kube.AppsV1().Deployments(ns).Delete(ctx, n, metav1.DeleteOptions{})
		}},
		{"service", directorDeploy, func() ([]string, error) {
			l, err := d.kube.CoreV1().Services(ns).List(ctx, sel)
			if err != nil {
				return nil, err
			}
			return names(len(l.Items), func(i int) string { return l.Items[i].Name }), nil
		}, func(ctx context.Context, n string) error {
			_, err := d.kube.CoreV1().Services(ns).Get(ctx, n, metav1.GetOptions{})
			return err
		}, func(ctx context.Context, n string) error {
			return d.kube.CoreV1().Services(ns).Delete(ctx, n, metav1.DeleteOptions{})
		}},
		{"rolebinding", directorDeploy, func() ([]string, error) {
			l, err := d.kube.RbacV1().RoleBindings(ns).List(ctx, sel)
			if err != nil {
				return nil, err
			}
			return names(len(l.Items), func(i int) string { return l.Items[i].Name }), nil
		}, func(ctx context.Context, n string) error {
			_, err := d.kube.RbacV1().RoleBindings(ns).Get(ctx, n, metav1.GetOptions{})
			return err
		}, func(ctx context.Context, n string) error {
			return d.kube.RbacV1().RoleBindings(ns).Delete(ctx, n, metav1.DeleteOptions{})
		}},
		{"role", directorDeploy, func() ([]string, error) {
			l, err := d.kube.RbacV1().Roles(ns).List(ctx, sel)
			if err != nil {
				return nil, err
			}
			return names(len(l.Items), func(i int) string { return l.Items[i].Name }), nil
		}, func(ctx context.Context, n string) error {
			_, err := d.kube.RbacV1().Roles(ns).Get(ctx, n, metav1.GetOptions{})
			return err
		}, func(ctx context.Context, n string) error {
			return d.kube.RbacV1().Roles(ns).Delete(ctx, n, metav1.DeleteOptions{})
		}},
		{"serviceaccount", directorDeploy, func() ([]string, error) {
			l, err := d.kube.CoreV1().ServiceAccounts(ns).List(ctx, sel)
			if err != nil {
				return nil, err
			}
			return names(len(l.Items), func(i int) string { return l.Items[i].Name }), nil
		}, func(ctx context.Context, n string) error {
			_, err := d.kube.CoreV1().ServiceAccounts(ns).Get(ctx, n, metav1.GetOptions{})
			return err
		}, func(ctx context.Context, n string) error {
			return d.kube.CoreV1().ServiceAccounts(ns).Delete(ctx, n, metav1.DeleteOptions{})
		}},
	}
	if !o.keepData {
		specs = append(specs, spec{"persistentvolumeclaim", directorDataClaim, func() ([]string, error) {
			l, err := d.kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, sel)
			if err != nil {
				return nil, err
			}
			return names(len(l.Items), func(i int) string { return l.Items[i].Name }), nil
		}, func(ctx context.Context, n string) error {
			_, err := d.kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, n, metav1.GetOptions{})
			return err
		}, func(ctx context.Context, n string) error {
			return d.kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, n, metav1.DeleteOptions{})
		}})
	}
	for _, s := range specs {
		var legacy []string
		if err := s.get(ctx, s.legacy); err == nil {
			legacy = []string{s.legacy}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get %s %s: %w", s.kind, s.legacy, err)
		}
		if err := add(s.kind, s.list, legacy, s.del); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func runSessionNames(ctx context.Context, d leaveDeps, ns string) ([]string, error) {
	l := &agentsv1.SessionList{}
	if err := d.ctrl.List(ctx, l, ctrlclient.InNamespace(ns), ctrlclient.HasLabels{runSessionLabel}); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	r := make([]string, 0, len(l.Items))
	for _, s := range l.Items {
		r = append(r, s.Name)
	}
	return r, nil
}

func runPodCount(ctx context.Context, d leaveDeps, ns string) (int, error) {
	l, err := d.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=tiny-session," + sessionPodLabel})
	if err != nil {
		return 0, fmt.Errorf("list pods: %w", err)
	}
	n := 0
	for _, p := range l.Items {
		if strings.HasPrefix(p.Labels[sessionPodLabel], runPodPrefix) {
			n++
		}
	}
	return n, nil
}

func confirm(in io.Reader, out io.Writer, q string) bool {
	fmt.Fprintf(out, "%s [y/N] ", q)
	line, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// runLeave removes everything boardingparty installed: run Sessions first (and
// their pods), then the director's objects.
func runLeave(ctx context.Context, d leaveDeps, o leaveOpts, out io.Writer) error {
	sessions, err := runSessionNames(ctx, d, o.ns)
	if err != nil {
		return err
	}
	pods, err := runPodCount(ctx, d, o.ns)
	if err != nil {
		return err
	}
	objs, err := collectObjects(ctx, d, o)
	if err != nil {
		return err
	}
	if len(sessions) == 0 && pods == 0 && len(objs) == 0 {
		fmt.Fprintln(out, "nothing to remove")
		return nil
	}
	fmt.Fprintf(out, "this will delete, in namespace %s:\n", clean(o.ns))
	for _, s := range sessions {
		fmt.Fprintf(out, "  session %s (run)\n", clean(s))
	}
	if pods > 0 && len(sessions) == 0 {
		fmt.Fprintf(out, "  %d leftover run pod(s) (waited for)\n", pods)
	}
	for _, ob := range objs {
		fmt.Fprintf(out, "  %s %s\n", ob.kind, clean(ob.name))
	}
	if o.keepData {
		fmt.Fprintf(out, "keeping %s (--keep-data)\n", directorDataClaim)
	}
	if !o.yes {
		if !o.interactive {
			return errors.New("refusing to delete without confirmation: pass --yes")
		}
		if !confirm(o.in, out, "Delete these?") {
			fmt.Fprintln(out, "nothing deleted")
			return nil
		}
	}

	deadline := time.Now().Add(o.timeout)
	if err := stopDirector(ctx, d, o, deadline, out); err != nil {
		return err
	}
	prop := metav1.DeletePropagationForeground
	for _, name := range sessions {
		s := &agentsv1.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: o.ns}}
		if err := d.ctrl.Delete(ctx, s, ctrlclient.PropagationPolicy(prop)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete session %s: %w", name, err)
		}
		fmt.Fprintf(out, "deleted session %s\n", clean(name))
	}
	if len(sessions) > 0 || pods > 0 {
		if err := waitRunsGone(ctx, d, o, deadline); err != nil {
			return err
		}
	}
	for _, ob := range objs {
		if err := ob.del(ctx); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", ob.kind, ob.name, err)
		}
		fmt.Fprintf(out, "deleted %s %s\n", ob.kind, clean(ob.name))
	}
	return nil
}

// stopDirector scales the director to zero and waits for its pods to go, so it
// can't start a new run while leave removes the old ones. A missing
// Deployment is skipped.
func stopDirector(ctx context.Context, d leaveDeps, o leaveOpts, deadline time.Time, out io.Writer) error {
	deps := d.kube.AppsV1().Deployments(o.ns)
	dep, err := deps.Get(ctx, directorDeploy, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", directorDeploy, err)
	}
	zero := int32(0)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		dep.Spec.Replicas = &zero
		if _, err := deps.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("scale deployment %s to 0: %w", directorDeploy, err)
		}
		fmt.Fprintf(out, "scaled deployment %s to 0\n", directorDeploy)
	}
	for {
		l, err := d.kube.CoreV1().Pods(o.ns).List(ctx, metav1.ListOptions{LabelSelector: directorSelector})
		if err != nil {
			return fmt.Errorf("list pods: %w", err)
		}
		if len(l.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %d director pod(s) to go", len(l.Items))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
}

func waitRunsGone(ctx context.Context, d leaveDeps, o leaveOpts, deadline time.Time) error {
	for {
		s, err := runSessionNames(ctx, d, o.ns)
		if err != nil {
			return err
		}
		p, err := runPodCount(ctx, d, o.ns)
		if err != nil {
			return err
		}
		if len(s) == 0 && p == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %d run session(s) and %d run pod(s) to go; director left in place", len(s), p)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
}

func newLeave(g *globals) *cobra.Command {
	var o leaveOpts
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Remove everything boardingparty installed",
		Long: "Stops any active run, waits for its Session and pods to go, then deletes the director\n" +
			"(Deployment, Service, RoleBinding, Role, ServiceAccount) and its data volume.\n" +
			"Nothing tiny owns is touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.chooseTarget(cmd.Context()); err != nil {
				return err
			}
			cfg, ns, err := g.kubeConfig()
			if err != nil {
				return err
			}
			d := leaveDeps{poll: 2 * time.Second}
			if d.kube, err = kubernetes.NewForConfig(cfg); err != nil {
				return err
			}
			sc := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(sc); err != nil {
				return err
			}
			if err := agentsv1.AddToScheme(sc); err != nil {
				return err
			}
			if d.ctrl, err = ctrlclient.New(cfg, ctrlclient.Options{Scheme: sc}); err != nil {
				return err
			}
			o.ns, o.timeout, o.in, o.interactive = ns, 3*time.Minute, os.Stdin, isTerminal()
			return runLeave(cmd.Context(), d, o, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&o.keepData, "keep-data", false, "keep the director's data volume")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "don't ask for confirmation (required without a terminal)")
	return cmd
}
