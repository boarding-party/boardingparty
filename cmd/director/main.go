// Command director runs the boardingparty control plane: it launches runs as
// tiny Sessions, serves the worker surface and the loopback-only admin API.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tiny-systems/boardingparty/internal/api"
	"github.com/tiny-systems/boardingparty/internal/artifacts"
	"github.com/tiny-systems/boardingparty/internal/launcher"
	"github.com/tiny-systems/boardingparty/internal/mcpserver"
	"github.com/tiny-systems/boardingparty/internal/runs"
	"github.com/tiny-systems/boardingparty/internal/store"
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

func main() {
	var cfg config
	flag.StringVar(&cfg.Data, "data", "/data", "data directory (SQLite + artifacts)")
	flag.StringVar(&cfg.Namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace for run Sessions (default $POD_NAMESPACE)")
	flag.StringVar(&cfg.WorkerImage, "worker-image", "", "worker image for run Sessions")
	flag.StringVar(&cfg.DirectorURL, "director-url", "http://bp-director:8080", "in-cluster URL workers use to reach the director")
	flag.StringVar(&cfg.Model, "model", "", "default model for runs")
	flag.StringVar(&cfg.AdminAddr, "admin-addr", "127.0.0.1:8081", "admin API listen address (keep on loopback)")
	flag.StringVar(&cfg.WorkerAddr, "worker-addr", ":8080", "worker listen address")
	flag.DurationVar(&cfg.RunTimeout, "run-timeout", runs.DefaultRunTimeout, "maximum run duration")
	flag.Int64Var(&cfg.MaxArtifactBytes, "max-artifact-bytes", 500<<20, "maximum artifact size in bytes")
	flag.Parse()

	if cfg.Namespace == "" {
		log.Fatal("namespace unknown: set --namespace or POD_NAMESPACE")
	}
	if cfg.WorkerImage == "" {
		log.Fatal("--worker-image is required")
	}

	st, err := store.Open(filepath.Join(cfg.Data, "boardingparty.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Plain (uncached) client: Sessions are unstructured and must be read live.
	kc, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
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
	servers := []*http.Server{
		{Addr: cfg.AdminAddr, Handler: admin, ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.WorkerAddr, Handler: worker, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			log.Printf("listening on %s", s.Addr)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(s)
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Printf("server: %v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
}
