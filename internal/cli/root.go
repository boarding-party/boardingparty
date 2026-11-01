// Package cli implements the boardingparty command line.
package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	directorSelector = "app=bp-director"
	adminPort        = 8081
)

type globals struct {
	kubeContext string
	namespace   string
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
	if d.apply, err = serverSideApplier(cfg); err != nil {
		return err
	}
	return ensureDirector(ctx, d, setupOpts{ns: ns, tag: wantedTag(), timeout: 3 * time.Minute}, os.Stderr)
}

// kubeConfig loads the kube credentials and resolves the target namespace.
func (g *globals) kubeConfig() (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: g.kubeContext})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kube config: %w", err)
	}
	ns := g.namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil || ns == "" {
			ns = "default"
		}
	}
	return cfg, ns, nil
}

// connect port-forwards to the director and returns a client plus cleanup.
func (g *globals) connect(ctx context.Context) (*client, func(), error) {
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
	g := &globals{}
	root := &cobra.Command{Use: "boardingparty", Short: "Drive boardingparty test runs on your cluster", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&g.kubeContext, "context", "", "kubeconfig context (default: current)")
	root.PersistentFlags().StringVarP(&g.namespace, "namespace", "n", "", "director namespace (default: the context's namespace, else default)")
	root.AddCommand(newSetup(g), newApply(g), newRun(g), newRuns(g), newGet(g))
	return root
}
