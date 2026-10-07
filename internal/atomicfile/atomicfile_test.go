package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWriteFileCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "file.txt")

	if err := WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("content = %q, want %q", got, "first")
	}

	// Replacing an existing file is the case that matters on Windows.
	if err := WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("WriteFile (replace) = %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "second" {
		t.Fatalf("content after replace = %q, want %q", got, "second")
	}
}

func TestWriteFileLeavesNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileModeIsRestrictive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits; ACLs govern access")
	}
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %v, want 0600", perm)
	}
}

func TestWriteFileReplacesDirectoryFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	// A directory in the destination's place cannot be replaced by a file.
	target := filepath.Join(dir, "blocked")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := WriteFile(target, []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFile over a directory = nil, want an error")
	}
	// The failure must not leave a temp file behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind after failure: %s", e.Name())
		}
	}
}

// TestWriteFileSurvivesConcurrentReplacement hammers the replace path from
// many goroutines. It is the regression test for the transient Windows
// rename failure ("Access is denied") that a single os.Rename produced.
func TestWriteFileSurvivesConcurrentReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := WriteFile(path, []byte("initial"), 0o600); err != nil {
		t.Fatalf("seed WriteFile = %v", err)
	}

	const workers = 16
	const rounds = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				payload := strings.Repeat("x", 64+id)
				if err := WriteFile(path, []byte(payload), 0o600); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent WriteFile failed: %v", err)
	}

	// Whatever won, the destination must be a complete file, never a
	// partial one and never absent.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after concurrent writes: %v", err)
	}
	if len(got) < 64 {
		t.Fatalf("content is truncated: %d bytes", len(got))
	}
}
