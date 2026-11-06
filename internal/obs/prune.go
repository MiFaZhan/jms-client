package obs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvAuditKeepDays is the retention window for the local audit files.
//
// "0" disables pruning. Unset means "use the configuration file", and the
// configuration default is also 0: deleting audit records is not something a
// tool should start doing because nobody said otherwise.
const EnvAuditKeepDays = "JMS_AUDIT_KEEP_DAYS"

// auditFilePrefix and auditFileSuffix bracket the per-pid file names
// (audit-YYYYMMDD-<pid>.jsonl).
const (
	auditFileNamePrefix = "audit-"
	auditFileNameSuffix = ".jsonl"
	// pruneStampName records the last automatic prune, so a process that
	// starts many times a minute does not rescan the directory each time.
	pruneStampName = ".prune-stamp"
	// pruneInterval is how often the automatic prune may run.
	pruneInterval = 24 * time.Hour
)

// ErrInvalidKeepDays reports a retention window that is not a positive
// number of days.
var ErrInvalidKeepDays = errors.New("keep days must be at least 1")

// PruneResult describes one prune run.
type PruneResult struct {
	// Deleted is how many files were (or would be) removed.
	Deleted int
	// Kept is how many files are inside the window.
	Kept int
	// Skipped is how many file names could not be dated and were left alone.
	Skipped int
	// FreedBytes is the size of the removed files.
	FreedBytes int64
	// Cutoff is the oldest date still kept, at local midnight.
	Cutoff time.Time
	// DryRun reports whether anything was actually removed.
	DryRun bool
}

// String renders a one-line summary for logs and command output.
func (r PruneResult) String() string {
	verb := "deleted"
	if r.DryRun {
		verb = "would delete"
	}
	return fmt.Sprintf("%s %d file(s), %d bytes; kept %d within the window (cutoff %s); skipped %d",
		verb, r.Deleted, r.FreedBytes, r.Kept, r.Cutoff.Format("20060102"), r.Skipped)
}

// PruneAuditFiles removes audit files older than keepDays.
//
// The age is read from the FILE NAME, not the modification time. A copied or
// restored directory resets mtime, and a prune that trusted mtime would then
// keep files that are years past the window — the direction of that mistake
// is "keeps too much", which nobody notices until the directory is huge.
//
// Files whose names cannot be dated are skipped rather than deleted: an
// unexpected name is a reason to look, not a reason to remove.
//
// A missing directory is not an error; it means nothing has been recorded
// yet.
func PruneAuditFiles(dir string, keepDays int, now time.Time, dryRun bool) (PruneResult, error) {
	if keepDays < 1 {
		return PruneResult{}, fmt.Errorf("%w (got %d)", ErrInvalidKeepDays, keepDays)
	}
	now = now.Local()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, -keepDays)
	result := PruneResult{Cutoff: cutoff, DryRun: dryRun}
	cutoffStamp := cutoff.Format("20060102")

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, fmt.Errorf("read audit directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, auditFileNamePrefix) ||
			!strings.HasSuffix(name, auditFileNameSuffix) {
			continue
		}
		stamp := auditFileDate(name)
		if stamp == "" {
			result.Skipped++
			continue
		}
		// YYYYMMDD sorts correctly as a string.
		if stamp >= cutoffStamp {
			result.Kept++
			continue
		}
		path := filepath.Join(dir, name)
		var size int64
		if info, statErr := entry.Info(); statErr == nil {
			size = info.Size()
		}
		if !dryRun {
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return result, fmt.Errorf("remove %s: %w", path, rmErr)
			}
		}
		result.Deleted++
		result.FreedBytes += size
	}
	return result, nil
}

// auditFileDate extracts YYYYMMDD from audit-YYYYMMDD-<pid>.jsonl.
//
// It returns "" when the name does not carry an eight-digit date, which the
// caller treats as "leave it alone".
func auditFileDate(name string) string {
	rest := strings.TrimPrefix(name, auditFileNamePrefix)
	rest = strings.TrimSuffix(rest, auditFileNameSuffix)
	idx := strings.Index(rest, "-")
	if idx <= 0 {
		return ""
	}
	stamp := rest[:idx]
	if len(stamp) != 8 {
		return ""
	}
	for i := 0; i < len(stamp); i++ {
		if stamp[i] < '0' || stamp[i] > '9' {
			return ""
		}
	}
	return stamp
}

// PruneIfDue runs PruneAuditFiles at most once per interval.
//
// A stamp file records the last run, so the long-lived MCP host and the
// short-lived CLI both prune without turning a directory scan into a
// per-command cost. A stamp that cannot be read or written is not fatal:
// pruning is housekeeping, and it must never be the reason a command fails.
func PruneIfDue(dir string, keepDays int, now time.Time, dryRun bool) (PruneResult, bool, error) {
	if keepDays < 1 {
		return PruneResult{}, false, nil
	}
	stampPath := filepath.Join(dir, pruneStampName)
	if !dryRun {
		if body, err := os.ReadFile(stampPath); err == nil {
			if last, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(string(body))); parseErr == nil {
				if now.Sub(last) < pruneInterval {
					return PruneResult{}, false, nil
				}
			}
		}
	}

	result, err := PruneAuditFiles(dir, keepDays, now, dryRun)
	if err != nil {
		return result, true, err
	}
	if dryRun {
		return result, true, nil
	}
	// Record the run even when nothing was deleted: the point of the stamp is
	// "we looked", not "we changed something".
	_ = os.WriteFile(stampPath, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	return result, true, nil
}

// ParseKeepDays accepts an integer number of days, including "0" for "off".
//
// It exists so the environment variable and the configuration file share one
// validation rule instead of each doing its own.
func ParseKeepDays(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("keep days %q is not a number", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("keep days %q must not be negative", raw)
	}
	return n, nil
}
