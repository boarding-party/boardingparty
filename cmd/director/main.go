// Command director runs the boardingparty control plane: it launches runs as
// tiny Sessions, serves the worker surface and the loopback-only admin API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1 "github.com/tiny-systems/tiny/api/v1alpha1"
	"github.com/tiny-systems/tiny/pkg/session"

	"github.com/boarding-party/boardingparty/internal/api"
	"github.com/boarding-party/boardingparty/internal/artifacts"
	"github.com/boarding-party/boardingparty/internal/launcher"
	"github.com/boarding-party/boardingparty/internal/mcpserver"
	"github.com/boarding-party/boardingparty/internal/runs"
	"github.com/boarding-party/boardingparty/internal/store"
)

type config struct {
	Data             string
	Namespace        string
	WorkerImage      string
	DirectorURL      string
	Model            string
	AdminAddr        string
	WorkerAddr       string
	RunTimeout       time.Duration
	MaxArtifactBytes int64
	TinyImageTag     string
}

const tinyModule = "github.com/tiny-systems/tiny"

// tinyVersion returns the version of the tiny module this binary was built
// against, from build info ("v0.11.0"); "" when unknown or not a release
// (a "(devel)" or replaced module).
func tinyVersion(bi *debug.BuildInfo) string {
	if bi == nil {
		return ""
	}
	for _, d := range bi.Deps {
		if d.Path != tinyModule {
			continue
		}
		if d.Replace != nil || d.Version == "(devel)" {
			return ""
		}
		return d.Version
	}
	return ""
}

// imageTag picks the tiny image tag: the build-info version of the tiny
// module (matching the agent and sidecar images published for that release),
// else the flag. tiny's SetImageTag accepts "v0.11.0" or "0.11.0" and ignores
// anything that is not a plain release.
func imageTag(bi *debug.BuildInfo, flagValue string) string {
	if v := tinyVersion(bi); v != "" {
		return v
	}
	return flagValue
}

func newScheme() (*runtime.Scheme, error) {
	sc := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sc); err != nil {
		return nil, err
	}
	if err := agentsv1.AddToScheme(sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// newServers builds the two handlers. The admin one (/api/*) must only be
// served on loopback; the worker one is reachable by worker pods and carries
// artifact upload, health and the per-run MCP endpoint, nothing from /api/*.
func newServers(cfg config, svc *runs.Service, st *store.Store) (admin, worker http.Handler) {
	admin = api.Admin(svc, cfg.MaxArtifactBytes)
	mux := http.NewServeMux()
	mux.Handle("/mcp/run/", mcpserver.Handler(svc, st, cfg.DirectorURL))
	mux.Handle("/", api.Worker(svc, cfg.MaxArtifactBytes))
	return admin, mux
}

// defaultDirectorURL is the in-cluster URL workers use. It must be a .svc
// name: tiny's egress proxy only bypasses .svc and .cluster.local hosts, so a
// bare service name would be proxied and refused.
func defaultDirectorURL(namespace string) string {
	return "http://bp-director." + namespace + ".svc:8080"
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.Data, "data", "/data", "data directory (SQLite + artifacts)")
	flag.StringVar(&cfg.Namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace for run Sessions (default $POD_NAMESPACE)")
	flag.StringVar(&cfg.WorkerImage, "worker-image", "", "worker image for run Sessions")
	flag.StringVar(&cfg.DirectorURL, "director-url", "", "in-cluster URL workers use to reach the director (default http://bp-director.<namespace>.svc:8080)")
	flag.StringVar(&cfg.Model, "model", "", "default model for runs")
	flag.StringVar(&cfg.AdminAddr, "admin-addr", "127.0.0.1:8081", "admin API listen address (keep on loopback)")
	flag.StringVar(&cfg.WorkerAddr, "worker-addr", ":8080", "worker listen address")
	flag.DurationVar(&cfg.RunTimeout, "run-timeout", runs.DefaultRunTimeout, "maximum run duration")
	flag.Int64Var(&cfg.MaxArtifactBytes, "max-artifact-bytes", 500<<20, "maximum artifact size in bytes")
	flag.StringVar(&cfg.TinyImageTag, "tiny-image-tag", "", "tiny agent/sidecar image tag, used only when build info has no tiny version (e.g. 0.11.0)")
	flag.Parse()

	if cfg.Namespace == "" {
		return errors.New("namespace unknown: set --namespace or POD_NAMESPACE")
	}
	if cfg.WorkerImage == "" {
		return errors.New("--worker-image is required")
	}
	if cfg.DirectorURL == "" {
		cfg.DirectorURL = defaultDirectorURL(cfg.Namespace)
	}

	st, err := store.Open(filepath.Join(cfg.Data, "boardingparty.db"))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	bi, _ := debug.ReadBuildInfo()
	session.SetImageTag(imageTag(bi, cfg.TinyImageTag))

	// Plain (uncached) client: Sessions and pods must be read live.
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	scheme, err := newScheme()
	if err != nil {
		return fmt.Errorf("scheme: %w", err)
	}
	kc, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	svc := &runs.Service{
		Store:     st,
		Artifacts: artifacts.NewFS(filepath.Join(cfg.Data, "artifacts"), cfg.MaxArtifactBytes),
		Launcher: &launcher.Tiny{
			Client: kc, Namespace: cfg.Namespace, WorkerImage: cfg.WorkerImage,
			DirectorURL: strings.TrimRight(cfg.DirectorURL, "/"), Model: cfg.Model,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// Re-adopt runs that were active before a restart.
	if err := svc.Reconcile(ctx, time.Now(), cfg.RunTimeout); err != nil {
		log.Printf("startup reconcile: %v", err)
	}
	go svc.Watch(ctx, runs.DefaultPollInterval, cfg.RunTimeout)

	admin, worker := newServers(cfg, svc, st)
	return serve(ctx, []*http.Server{
		{Addr: cfg.AdminAddr, Handler: admin, ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.WorkerAddr, Handler: worker, ReadHeaderTimeout: 10 * time.Second},
	})
}

// serve runs the servers until ctx ends or one fails to listen/serve, then
// shuts all of them down. It returns the first listener error, if any.
func serve(ctx context.Context, servers []*http.Server) error {
	errc := make(chan error, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			errc <- fmt.Errorf("listen %s: %w", s.Addr, err)
			continue
		}
		go func(s *http.Server) {
			log.Printf("listening on %s", s.Addr)
			if err := s.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve %s: %w", s.Addr, err)
			}
		}(s)
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return result
}
