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
	group.AddCommand(newLogPruneCommand(deps))
	return group
}

// newLogPruneCommand builds `jms log prune`.
//
// It exists next to the automatic prune because both are useful: the
// automatic one enforces the window without anyone remembering to, and this
// one answers "what would be deleted" and "delete it now" without waiting for
// the next process start.
func newLogPruneCommand(deps *Deps) *cobra.Command {
	var (
		keep   int
		dryRun bool
		dir    string
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete local audit files older than the retention window",
		Long: `Delete local audit files older than the retention window.

The window comes from --keep, else JMS_AUDIT_KEEP_DAYS, else audit_keep_days
in the configuration file. The age is read from the file name
(audit-YYYYMMDD-<pid>.jsonl), not from the modification time, so a copied
directory does not silently keep files forever.

Two safety rules: a file whose name carries no date is skipped, not deleted;
and without a configured window this command does nothing and says so.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogPrune(cmd, *deps, keep, dryRun, dir)
		},
	}
	cmd.Flags().IntVar(&keep, "keep", 0, "Keep this many days (overrides the configured window)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be deleted without deleting")
	cmd.Flags().StringVar(&dir, "dir", "", "Audit directory (default: the configured one)")
	return cmd
}

// runLogPrune applies the retention window.
func runLogPrune(cmd *cobra.Command, deps Deps, keep int, dryRun bool, dir string) error {
	target := strings.TrimSpace(dir)
	if target == "" {
		target = deps.Runtime.AuditDir
	}
	days := keep
	if days <= 0 {
		days = deps.Runtime.AuditKeepDays
	}
	if days <= 0 {
		return fmt.Errorf("no retention window configured: pass --keep <days>, set %s, or add audit_keep_days to the configuration file",
			obs.EnvAuditKeepDays)
	}
	result, err := obs.PruneAuditFiles(target, days, deps.Now(), dryRun)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(deps.Out, "jms: audit prune in %s: %s\n", target, result); err != nil {
		return err
	}
	return nil
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
