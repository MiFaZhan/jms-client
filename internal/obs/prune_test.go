package obs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAuditDir builds a temp audit directory holding one file per name.
//
// The name avoids colliding with auditDir, the package function that
// resolves the configured directory.
func newAuditDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"kind":"exec.end"}`+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestPruneAuditFilesByFileNameDate(t *testing.T) {
	t.Parallel()

	dir := newAuditDir(t,
		"audit-20260101-1.jsonl", // well outside the window
		"audit-20260807-2.jsonl", // one day before the cutoff
		"audit-20260808-3.jsonl", // exactly the cutoff: kept
		"audit-20261106-4.jsonl", // today
		"audit-notadate-5.jsonl", // unparsable: skipped
		"notes.txt",              // not an audit file: untouched
	)
	now := time.Date(2026, 11, 6, 17, 0, 0, 0, time.Local)

	res, err := PruneAuditFiles(dir, 90, now, false)
	if err != nil {
		t.Fatalf("PruneAuditFiles: %v", err)
	}
	if res.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (2026-01-01 and 2026-08-07)", res.Deleted)
	}
	if res.Kept != 2 {
		t.Errorf("kept = %d, want 2 (cutoff day and today)", res.Kept)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the undated name)", res.Skipped)
	}
	if got := res.Cutoff.Format("20060102"); got != "20260808" {
		t.Errorf("cutoff = %s, want 20260808", got)
	}

	for _, name := range []string{"audit-20260807-2.jsonl", "audit-20260101-1.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after the prune", name)
		}
	}
	for _, name := range []string{"audit-20260808-3.jsonl", "audit-20261106-4.jsonl", "audit-notadate-5.jsonl", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed but should have been kept: %v", name, err)
		}
	}
}

func TestPruneAuditFilesDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	dir := newAuditDir(t, "audit-20200101-1.jsonl")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)

	res, err := PruneAuditFiles(dir, 90, now, true)
	if err != nil {
		t.Fatalf("PruneAuditFiles: %v", err)
	}
	if res.Deleted != 1 || !res.DryRun {
		t.Fatalf("result = %+v, want one would-be deletion", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit-20200101-1.jsonl")); err != nil {
		t.Fatalf("dry run removed the file: %v", err)
	}
	if !strings.Contains(res.String(), "would delete") {
		t.Errorf("summary = %q", res.String())
	}
}

func TestPruneAuditFilesRejectsBadWindow(t *testing.T) {
	t.Parallel()

	dir := newAuditDir(t, "audit-20200101-1.jsonl")
	for _, keep := range []int{0, -1} {
		if _, err := PruneAuditFiles(dir, keep, time.Now(), false); err == nil {
			t.Fatalf("keep=%d was accepted", keep)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "audit-20200101-1.jsonl")); err != nil {
		t.Fatalf("a rejected window deleted a file: %v", err)
	}
}

func TestPruneAuditFilesMissingDir(t *testing.T) {
	t.Parallel()

	res, err := PruneAuditFiles(filepath.Join(t.TempDir(), "absent"), 90, time.Now(), false)
	if err != nil {
		t.Fatalf("a missing directory is not an error: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d on an absent directory", res.Deleted)
	}
}

func TestAuditFileDate(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"audit-20261106-817387.jsonl": "20261106",
		"audit-20260101-1.jsonl":      "20260101",
		"audit-notadate-5.jsonl":      "",
		"audit-2026110-5.jsonl":       "",
		"audit-20261106.jsonl":        "",
		"other-20261106-1.jsonl":      "",
		"audit-2026/106-1.jsonl":      "",
	}
	for name, want := range cases {
		if got := auditFileDate(name); got != want {
			t.Errorf("auditFileDate(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPruneIfDueRunsOncePerInterval(t *testing.T) {
	t.Parallel()

	dir := newAuditDir(t, "audit-20200101-1.jsonl")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)

	res, ran, err := PruneIfDue(dir, 90, now, false)
	if err != nil || !ran {
		t.Fatalf("first call: ran=%v err=%v", ran, err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", res.Deleted)
	}

	// A second process starting minutes later must not rescan: the stamp says
	// this was handled.
	if _, ran, err := PruneIfDue(dir, 90, now.Add(time.Minute), false); err != nil || ran {
		t.Fatalf("second call within the interval: ran=%v err=%v, want ran=false", ran, err)
	}

	// After the interval it runs again, even with nothing to delete.
	if _, ran, err := PruneIfDue(dir, 90, now.Add(25*time.Hour), false); err != nil || !ran {
		t.Fatalf("call after the interval: ran=%v err=%v, want ran=true", ran, err)
	}

	// A dry run must not consume the window.
	if _, ran, err := PruneIfDue(dir, 90, now.Add(50*time.Hour), true); err != nil || !ran {
		t.Fatalf("dry run: ran=%v err=%v", ran, err)
	}
	if _, ran, err := PruneIfDue(dir, 90, now.Add(50*time.Hour), false); err != nil || !ran {
		t.Fatalf("a dry run stamped the interval, so the next real run was skipped: ran=%v err=%v", ran, err)
	}
}

func TestPruneIfDueDisabledWindow(t *testing.T) {
	t.Parallel()

	dir := newAuditDir(t, "audit-20200101-1.jsonl")
	if _, ran, err := PruneIfDue(dir, 0, time.Now(), false); err != nil || ran {
		t.Fatalf("ran=%v err=%v, want a no-op for keepDays=0", ran, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit-20200101-1.jsonl")); err != nil {
		t.Fatalf("keepDays=0 deleted a file: %v", err)
	}
}

func TestParseKeepDays(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		want int
		err  bool
	}{
		"":       {0, false},
		"90":     {90, false},
		"0":      {0, false},
		" 30 ":   {30, false},
		"-5":     {0, true},
		"ninety": {0, true},
	}
	for raw, want := range cases {
		got, err := ParseKeepDays(raw)
		if (err != nil) != want.err {
			t.Errorf("ParseKeepDays(%q) err = %v, want err=%v", raw, err, want.err)
			continue
		}
		if got != want.want {
			t.Errorf("ParseKeepDays(%q) = %d, want %d", raw, got, want.want)
		}
	}
}
