package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/obs"
)

// auditFilePrefix and auditFileSuffix bracket the per-pid audit file names
// written by obs.AuditWriter: audit-YYYYMMDD-<pid>.jsonl (DESIGN.md「审计日志」).
const (
	auditFilePrefix = "audit-"
	auditFileSuffix = ".jsonl"
)

// newTailCommand builds `jms tail`: read the local audit stream.
func newTailCommand(deps *Deps) *cobra.Command {
	var (
		follow bool
		asJSON bool
		last   int
	)
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Follow the local audit stream",
		Long: `Show the local audit stream: what ran, on which asset, and how it ended.

The stream is the per-process JSONL files under the audit directory, so it
works whether or not a host is running. Several MCP clients writing at once
produce several files, which this command merges by timestamp.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTail(cmd, *deps, tailOptions{follow: follow, asJSON: asJSON, last: last})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep following")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit raw JSONL")
	cmd.Flags().IntVar(&last, "last", 0, "Start with the last N events")
	return cmd
}

// tailOptions carries the parsed `jms tail` flags.
type tailOptions struct {
	follow bool
	asJSON bool
	last   int
}

// runTail prints the audit events and, with -f, keeps printing new ones.
//
// The read is a poll rather than a filesystem watch: an audit file has
// exactly one writer (its own process), so tailing by offset is enough, and
// polling keeps the implementation portable across Windows and Unix.
func runTail(cmd *cobra.Command, deps Deps, opts tailOptions) error {
	dir := deps.Runtime.AuditDir
	events, offsets, err := readAuditEvents(dir)
	if err != nil {
		return err
	}

	if opts.last > 0 && len(events) > opts.last {
		events = events[len(events)-opts.last:]
	}
	for _, e := range events {
		if err := printAuditEvent(deps.Out, e, opts.asJSON); err != nil {
			return err
		}
	}
	if !opts.follow {
		return nil
	}

	ctx := commandContext(cmd)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			fresh, next, err := readAuditEventsFrom(dir, offsets)
			offsets = next
			if err != nil {
				return err
			}
			for _, e := range fresh {
				if err := printAuditEvent(deps.Out, e, opts.asJSON); err != nil {
					return err
				}
			}
		}
	}
}

// printAuditEvent renders one event: raw JSONL with --json, a compact
// one-line summary otherwise.
func printAuditEvent(out io.Writer, e obs.Event, asJSON bool) error {
	if asJSON {
		line, err := obs.MarshalLine(e)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(line))
		return err
	}
	_, err := fmt.Fprintln(out, renderAuditEvent(e))
	return err
}

// renderAuditEvent renders one event as a single line, matching the shape
// DESIGN.md「IPC 宿主」 shows for the attach view.
func renderAuditEvent(e obs.Event) string {
	var b strings.Builder
	b.WriteString(e.TS.Format("15:04:05"))

	if e.Server != "" || e.Asset != "" {
		b.WriteString("  ")
		b.WriteString(e.Server)
		if e.Asset != "" {
			if e.Server != "" {
				b.WriteString("/")
			}
			b.WriteString(e.Asset)
		}
	}
	if e.Backend != "" {
		b.WriteString("  " + e.Backend)
	}
	if e.Endpoint != "" {
		b.WriteString("  " + endpointBadge(e.Endpoint))
	}
	if e.Pool != "" {
		b.WriteString("  " + poolBadge(e.Pool))
	}
	b.WriteString("  " + string(e.Kind))

	if e.Actor != "" {
		b.WriteString("  [" + e.Actor + "]")
	}
	if e.Command != "" {
		b.WriteString("  $ " + e.Command)
	}
	if e.ExitCode != nil {
		b.WriteString(fmt.Sprintf("  exit %d", *e.ExitCode))
	}
	if e.DurationMS > 0 {
		b.WriteString(fmt.Sprintf("  %dms", e.DurationMS))
	}
	if e.OutputBytes > 0 {
		b.WriteString(fmt.Sprintf("  %dB", e.OutputBytes))
	}
	if e.Error != "" {
		b.WriteString("  ! " + e.Error)
	}
	if e.Reason != "" && e.Command == "" {
		b.WriteString("  " + e.Reason)
	}
	return b.String()
}

// endpointBadge renders the internal/external marker DESIGN.md「端点故障转移」第 6 点
// asks for in the tail and attach output.
func endpointBadge(kind string) string {
	switch strings.ToLower(kind) {
	case "internal":
		return "[内]"
	case "external":
		return "[外]"
	default:
		return "[" + kind + "]"
	}
}

// poolBadge renders a cold-start or pool-hit marker.
func poolBadge(pool string) string {
	switch strings.ToLower(pool) {
	case "cold":
		return "⚡冷连"
	case "hit":
		return "♻命中"
	default:
		return pool
	}
}

// readAuditEvents reads every audit file and returns the events sorted by
// timestamp, plus the byte offset consumed per file so a follow can resume.
func readAuditEvents(dir string) ([]obs.Event, map[string]int64, error) {
	return readAuditEventsFrom(dir, nil)
}

// readAuditEventsFrom reads new events from dir, starting each file at the
// offset already consumed for it.
//
// A missing directory is not an error: it means nothing has been recorded
// yet, which a caller reports as an empty stream.
func readAuditEventsFrom(dir string, offsets map[string]int64) ([]obs.Event, map[string]int64, error) {
	files, err := auditFiles(dir)
	if err != nil {
		return nil, offsets, err
	}
	next := make(map[string]int64, len(files))
	var events []obs.Event
	for _, path := range files {
		start := int64(0)
		if offsets != nil {
			start = offsets[path]
		}
		parsed, consumed, err := readAuditFile(path, start)
		if err != nil {
			return nil, offsets, err
		}
		next[path] = consumed
		events = append(events, parsed...)
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].TS.Before(events[j].TS)
	})
	return events, next, nil
}

// auditFiles lists the audit JSONL files in dir, oldest name first (the name
// carries the date and the pid).
func auditFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read audit directory %s: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, auditFilePrefix) ||
			!strings.HasSuffix(name, auditFileSuffix) {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files, nil
}

// readAuditFile reads whole lines from path starting at start, returning the
// parsed events and the offset after the last complete line.
//
// A trailing partial line is left unconsumed: the writer may be mid-append,
// and counting it would both drop it and corrupt the next event.
func readAuditFile(path string, start int64) ([]obs.Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, start, nil
		}
		return nil, start, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, start, fmt.Errorf("seek %s: %w", path, err)
	}

	reader := bufio.NewReader(f)
	var events []obs.Event
	consumed := start
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Leave a partial trailing line for the next read.
				return events, consumed, nil
			}
			return nil, start, fmt.Errorf("read %s: %w", path, err)
		}
		consumed += int64(len(line))

		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var e obs.Event
		if err := json.Unmarshal([]byte(trimmed), &e); err != nil {
			// A malformed line must not stop the stream: skip it.
			continue
		}
		events = append(events, e)
	}
}
