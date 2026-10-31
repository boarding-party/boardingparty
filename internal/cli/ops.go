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
	"unicode"
)

// clean replaces control runes (C0, DEL, C1) except tab with '?', so
// server-supplied text cannot inject terminal escape sequences.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// formatRuns writes the runs table. Tokens are never printed.
func formatRuns(w io.Writer, runs []Run, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPERSONA\tSTORY\tSTATE\tAGE\tOUTCOME")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(r.ID), clean(r.Persona), clean(r.Story), clean(r.State), age(now, r.Started), clean(oneLine(r.Outcome)))
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

const maxPollErrors = 3

func terminal(state string) bool {
	return state == "done" || state == "stuck" || state == "failed"
}

// waitRun polls until the run reaches a terminal state or ctx ends.
func waitRun(ctx context.Context, c *client, id string, every time.Duration) (Run, error) {
	fails := 0
	for {
		d, err := c.RunDetail(ctx, id)
		if err != nil {
			fails++
			if fails > maxPollErrors || ctx.Err() != nil {
				return Run{}, err
			}
			fmt.Fprintf(os.Stderr, "poll error (%d/%d): %v\n", fails, maxPollErrors, err)
			d = RunDetail{}
		} else if fails = 0; terminal(d.Run.State) {
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
	fmt.Fprintf(w, "Run:      %s\nPersona:  %s\nStory:    %s\nState:    %s\nOutcome:  %s\n", clean(r.ID), clean(r.Persona), clean(r.Story), clean(r.State), clean(r.Outcome))
	fmt.Fprintf(w, "Bugs:     %d\n", len(d.Bugs))
	for _, b := range d.Bugs {
		fmt.Fprintf(w, "  [%s] %s (step: %s)\n", clean(b.Severity), clean(b.Summary), clean(b.Step))
		if b.Note != "" {
			fmt.Fprintf(w, "      %s\n", clean(b.Note))
		}
		if b.Evidence != "" {
			fmt.Fprintf(w, "      evidence: %s\n", clean(b.Evidence))
		}
	}
	fmt.Fprintf(w, "Artifacts: %d\n", len(d.Artifacts))
	for _, a := range d.Artifacts {
		fmt.Fprintf(w, "  %s\n", clean(a))
	}
}

// downloadArtifacts writes every artifact into dir, refusing unsafe names.
func downloadArtifacts(ctx context.Context, c *client, d RunDetail, dir string) error {
	for _, name := range d.Artifacts {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
			return fmt.Errorf("refusing artifact with unsafe name %q", clean(name))
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
