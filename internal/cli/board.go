package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"

	"github.com/boarding-party/boardingparty/internal/board"
)

// kubeForwarder is board.Forwarder over a kube port-forward to the director pod.
type kubeForwarder struct {
	cfg *rest.Config
	ns  string

	mu   sync.Mutex
	stop func()
}

func (f *kubeForwarder) Open(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stop != nil {
		f.stop()
		f.stop = nil
	}
	base, stop, err := forward(ctx, f.cfg, f.ns, directorSelector, adminPort)
	if err != nil {
		return "", err
	}
	f.stop = stop
	return base, nil
}

func (f *kubeForwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stop != nil {
		f.stop()
		f.stop = nil
	}
}

// openBrowser opens url in the default browser; a variable so tests don't.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

type boardOpts struct {
	port   int
	noOpen bool
}

// runBoard serves the UI until ctx is done, then shuts the server and the
// forward down. The director must already be installed.
func runBoard(ctx context.Context, fw board.Forwarder, o boardOpts, out io.Writer) error {
	p := board.NewProxy(fw)
	if err := p.Start(ctx); err != nil {
		return fmt.Errorf("connect to the director: %w", err)
	}
	defer p.Close()
	fmt.Fprintln(out, "✓ director ready")

	token, err := board.NewToken()
	if err != nil {
		return err
	}
	ui, err := board.UI()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.port)))
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w", o.port, err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	srv := &http.Server{
		Handler:           board.NewHandler(board.Config{Token: token, Port: port, Proxy: p, UI: ui}),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()

	// The only place the token is ever printed.
	url := fmt.Sprintf("http://127.0.0.1:%d/?key=%s", port, token)
	fmt.Fprintf(out, "→ %s\n", url)
	if !o.noOpen {
		if err := openBrowser(url); err != nil {
			fmt.Fprintln(out, "couldn't open a browser; open the link above")
		}
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close() // a video stream still open; cut it
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newBoard(g *globals) *cobra.Command {
	var o boardOpts
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Open the boardingparty UI in your browser",
		Long: "Serves the UI on 127.0.0.1 and connects it to the director. The printed link\n" +
			"carries a one-off key; anyone without it gets nothing. Ctrl-C stops it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := g.chooseTarget(ctx); err != nil {
				return err
			}
			cfg, ns, err := g.kubeConfig()
			if err != nil {
				return err
			}
			if err := g.ensure(ctx, cfg, ns); err != nil {
				return err
			}
			return runBoard(ctx, &kubeForwarder{cfg: cfg, ns: ns}, o, cmd.OutOrStdout())
		},
	}
	cmd.Flags().IntVar(&o.port, "port", 0, "local port (0 = any free port)")
	cmd.Flags().BoolVar(&o.noOpen, "no-open", false, "don't open a browser")
	return cmd
}
