package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// ExitError carries a process exit code.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

var pollEvery = 5 * time.Second

func newApply(g *globals) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use: "apply -f project.yaml", Short: "Upload the project definition", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("-f is required")
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			c, stop, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer stop()
			if err := c.PutProject(cmd.Context(), b); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "project applied")
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "project YAML file")
	return cmd
}

// runAndWait is the testable core of `run`.
func runAndWait(ctx context.Context, c *client, out io.Writer, persona, story string, wait bool, timeout time.Duration) error {
	r, err := c.StartRun(ctx, persona, story)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run %s started (token %s)\n", clean(r.ID), clean(r.Token))
	if !wait {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fin, err := waitRun(wctx, c, r.ID, pollEvery)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run %s %s: %s\n", clean(fin.ID), clean(fin.State), clean(fin.Outcome))
	if fin.State != "done" {
		return ExitError{Code: 1}
	}
	return nil
}

func newRun(g *globals) *cobra.Command {
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use: "run <persona> <story>", Short: "Start a run", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, stop, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer stop()
			return runAndWait(cmd.Context(), c, cmd.OutOrStdout(), args[0], args[1], wait, timeout)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run finishes; exit 1 unless it is done")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "max time to wait with --wait")
	return cmd
}

func newRuns(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "runs", Short: "List recent runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, stop, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer stop()
			rs, err := c.Runs(cmd.Context())
			if err != nil {
				return err
			}
			formatRuns(cmd.OutOrStdout(), rs, time.Now())
			return nil
		},
	}
}

func newGet(g *globals) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use: "get <run-id>", Short: "Show a run and its bugs; download artifacts with --out", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, stop, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer stop()
			d, err := c.RunDetail(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printDetail(cmd.OutOrStdout(), d)
			if out != "" {
				if err := downloadArtifacts(cmd.Context(), c, d, out); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "artifacts written to %s\n", out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "directory to download artifacts into")
	return cmd
}
