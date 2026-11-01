// Package cli implements the boardingparty command line.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	directorSelector = "app=bp-director"
	adminPort        = 8081
)

type globals struct {
	kubeContext string
	namespace   string
	resolved    bool
}

// isTerminal reports whether stdin, stdout and stderr (where the picker draws) are terminals; a variable
// so tests can force either answer.
var isTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// chooseTarget settles the context and namespace before anything else touches
// the cluster: it asks for what the flags left out (on a terminal), remembers
// the answer and prints the target. It runs once per process.
func (g *globals) chooseTarget(ctx context.Context) error {
	if g.resolved {
		return nil
	}
	d := targetDeps{
		interactive: isTerminal(),
		contexts:    kubeContexts,
		namespaces: func(ctx context.Context, kubeContext string) ([]nsItem, error) {
			cfg, err := loadRestConfig(kubeContext)
			if err != nil {
				return nil, err
			}
			kc, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return nil, err
			}
			return listNamespaces(ctx, kc)
		},
		prompt: termPrompter{}, load: loadTarget, save: saveTarget, out: os.Stderr,
	}
	t, err := resolveTarget(ctx, d, g.kubeContext, g.namespace)
	if err != nil {
		return err
	}
	g.kubeContext, g.namespace, g.resolved = t.Context, t.Namespace, true
	return nil
}

// loadRestConfig builds credentials for a kubeconfig context ("" = current).
func loadRestConfig(kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	cfg.WarningHandler = rest.NoWarnings{}
	return cfg, nil
}

// quietClientGo keeps client-go's own logging (klog, API warnings) off the
// terminal; the CLI reports what matters itself.
func quietClientGo() {
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
	rest.SetDefaultWarningHandler(rest.NoWarnings{})
}

// ensure installs or upgrades the director on first use. Progress goes to stderr.
func (g *globals) ensure(ctx context.Context, cfg *rest.Config, ns string) error {
	d := setupDeps{}
	var err error
	if d.kube, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	if d.ext, err = apiextclient.NewForConfig(cfg); err != nil {
		return err
	}
	if d.apply, err = serverSideApplier(cfg, false); err != nil {
		return err
	}
	d.verify = verifyDirector(cfg, ns)
	return ensureDirector(ctx, d, setupOpts{ns: ns, tag: wantedTag(), timeout: 3 * time.Minute}, os.Stderr)
}

// kubeConfig loads the kube credentials for the chosen target.
func (g *globals) kubeConfig() (*rest.Config, string, error) {
	cfg, err := loadRestConfig(g.kubeContext)
	if err != nil {
		return nil, "", err
	}
	return cfg, g.namespace, nil
}

// connect port-forwards to the director and returns a client plus cleanup.
func (g *globals) connect(ctx context.Context) (*client, func(), error) {
	if err := g.chooseTarget(ctx); err != nil {
		return nil, nil, err
	}
	cfg, ns, err := g.kubeConfig()
	if err != nil {
		return nil, nil, err
	}
	if err := g.ensure(ctx, cfg, ns); err != nil {
		return nil, nil, err
	}
	base, stop, err := forward(ctx, cfg, ns, directorSelector, adminPort)
	if err != nil {
		return nil, nil, err
	}
	return newClient(base), stop, nil
}

// NewRoot builds the root command.
func NewRoot() *cobra.Command {
	quietClientGo()
	g := &globals{}
	root := &cobra.Command{Use: "boardingparty", Short: "Drive boardingparty test runs on your cluster", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&g.kubeContext, "context", "", "kubeconfig context (asked for on a terminal; default: current)")
	root.PersistentFlags().StringVarP(&g.namespace, "namespace", "n", "", "director namespace (asked for on a terminal; required elsewhere)")
	root.AddCommand(newSetup(g), newApply(g), newRun(g), newRuns(g), newGet(g))
	return root
}
