package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// formatRuns writes the runs table. Tokens are never printed.
func formatRuns(w io.Writer, runs []Run, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPERSONA\tSTORY\tSTATE\tAGE\tOUTCOME")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Persona, r.Story, r.State, age(now, r.Started), oneLine(r.Outcome))
	}
	tw.Flush()
}

func age(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func terminal(state string) bool {
	return state == "done" || state == "stuck" || state == "failed"
}

// waitRun polls until the run reaches a terminal state or ctx ends.
func waitRun(ctx context.Context, c *client, id string, every time.Duration) (Run, error) {
	for {
		d, err := c.RunDetail(ctx, id)
		if err != nil {
			return Run{}, err
		}
		if terminal(d.Run.State) {
			return d.Run, nil
		}
		select {
		case <-ctx.Done():
			return d.Run, fmt.Errorf("timed out waiting for run %s (state %s)", id, d.Run.State)
		case <-time.After(every):
		}
	}
}

// printDetail prints a run and its bugs (no token).
func printDetail(w io.Writer, d RunDetail) {
	r := d.Run
	fmt.Fprintf(w, "Run:      %s\nPersona:  %s\nStory:    %s\nState:    %s\nOutcome:  %s\n", r.ID, r.Persona, r.Story, r.State, r.Outcome)
	fmt.Fprintf(w, "Bugs:     %d\n", len(d.Bugs))
	for _, b := range d.Bugs {
		fmt.Fprintf(w, "  [%s] %s (step: %s)\n", b.Severity, b.Summary, b.Step)
		if b.Note != "" {
			fmt.Fprintf(w, "      %s\n", b.Note)
		}
		if b.Evidence != "" {
			fmt.Fprintf(w, "      evidence: %s\n", b.Evidence)
		}
	}
	fmt.Fprintf(w, "Artifacts: %d\n", len(d.Artifacts))
	for _, a := range d.Artifacts {
		fmt.Fprintf(w, "  %s\n", a)
	}
}

// downloadArtifacts writes every artifact into dir, refusing unsafe names.
func downloadArtifacts(ctx context.Context, c *client, d RunDetail, dir string) error {
	for _, name := range d.Artifacts {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
			return fmt.Errorf("refusing artifact with unsafe name %q", name)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range d.Artifacts {
		if err := fetchTo(ctx, c, d.Run.ID, name, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func fetchTo(ctx context.Context, c *client, id, name, path string) error {
	rc, err := c.Artifact(ctx, id, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
