// Package cli implements the boardingparty command line.
package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
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

// connect port-forwards to the director and returns a client plus cleanup.
func (g *globals) connect(ctx context.Context) (*client, func(), error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: g.kubeContext})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("kube config: %w", err)
	}
	ns := g.namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil || ns == "" {
			ns = "default"
		}
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
	root := &cobra.Command{Use: "boardingparty", Short: "Drive boardingparty test runs on your cluster", SilenceUsage: true}
	root.PersistentFlags().StringVar(&g.kubeContext, "context", "", "kubeconfig context (default: current)")
	root.PersistentFlags().StringVarP(&g.namespace, "namespace", "n", "", "director namespace (default: the context's namespace, else default)")
	root.AddCommand(newApply(g), newRun(g), newRuns(g), newGet(g))
	return root
}
