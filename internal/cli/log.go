package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/obs"
)

// newLogCommand builds `jms log show`.
func newLogCommand(deps *Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "log",
		Short: "Query the audit log",
	}
	var (
		since  string
		asset  string
		failed bool
		export string
	)
	show := &cobra.Command{
		Use:   "show",
		Short: "Show recorded audit events",
		Long: `Show the recorded audit events, newest last.

Unlike ` + "`jms tail`" + `, this reads the log as a file: it returns immediately and
is meant for looking back over a window rather than watching live.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogShow(cmd, *deps, logOptions{
				since:  since,
				asset:  asset,
				failed: failed,
				export: export,
			})
		},
	}
	show.Flags().StringVar(&since, "since", "", "Only events newer than this (e.g. 1h, 30m)")
	show.Flags().StringVar(&asset, "asset", "", "Only this asset")
	show.Flags().BoolVar(&failed, "failed", false, "Only failed commands")
	show.Flags().StringVar(&export, "export", "", "Export format: md")
	group.AddCommand(show)
	return group
}

// logOptions carries the parsed `jms log show` flags.
type logOptions struct {
	since  string
	asset  string
	failed bool
	export string
}

// runLogShow reads the audit files, filters them and prints the result.
func runLogShow(cmd *cobra.Command, deps Deps, opts logOptions) error {
	events, _, err := readAuditEvents(deps.Runtime.AuditDir)
	if err != nil {
		return err
	}

	cutoff := time.Time{}
	if opts.since != "" {
		d, err := parseSince(opts.since)
		if err != nil {
			return err
		}
		cutoff = deps.Now().Add(-d)
	}

	filtered := events[:0]
	for _, e := range events {
		if !cutoff.IsZero() && e.TS.Before(cutoff) {
			continue
		}
		if opts.asset != "" && e.Asset != opts.asset {
			continue
		}
		if opts.failed && !isFailure(e) {
			continue
		}
		filtered = append(filtered, e)
	}

	switch strings.ToLower(opts.export) {
	case "":
		for _, e := range filtered {
			if err := printAuditEvent(deps.Out, e, false); err != nil {
				return err
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintln(deps.Out, "No audit events matched.")
		}
		return nil
	case "md", "markdown":
		return writeMarkdownLog(deps.Out, filtered)
	default:
		return fmt.Errorf("unsupported export format %q: use md", opts.export)
	}
}

// isFailure reports whether an event records a failed command.
func isFailure(e obs.Event) bool {
	if e.Error != "" {
		return true
	}
	return e.ExitCode != nil && *e.ExitCode != 0
}

// parseSince accepts a Go duration ("1h", "30m") or a bare number of hours.
func parseSince(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if d, err := time.ParseDuration(trimmed); err == nil {
		return d, nil
	}
	return 0, fmt.Errorf("--since %q is not a duration: use a form like 1h or 30m", raw)
}

// writeMarkdownLog renders the filtered events as a Markdown table, for
// pasting into a report or a wiki page.
func writeMarkdownLog(out io.Writer, events []obs.Event) error {
	if _, err := fmt.Fprintln(out, "| Time | Server | Asset | Command | Exit | Duration | Actor |"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "| --- | --- | --- | --- | --- | --- | --- |"); err != nil {
		return err
	}
	for _, e := range events {
		exit := ""
		if e.ExitCode != nil {
			exit = fmt.Sprintf("%d", *e.ExitCode)
		}
		duration := ""
		if e.DurationMS > 0 {
			duration = fmt.Sprintf("%dms", e.DurationMS)
		}
		if _, err := fmt.Fprintf(out, "| %s | %s | %s | %s | %s | %s | %s |\n",
			e.TS.Format(time.RFC3339), markdownCell(e.Server), markdownCell(e.Asset),
			markdownCell(e.Command), exit, duration, markdownCell(e.Actor)); err != nil {
			return err
		}
	}
	return nil
}

// markdownCell escapes the characters that would break a table cell.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
