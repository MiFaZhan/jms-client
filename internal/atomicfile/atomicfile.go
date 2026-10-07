// Package atomicfile writes files so that readers never observe a partial
// or mixed state.
//
// It exists because two packages (config, endpoint) need the same
// write-temp-then-rename discipline, and because the rename step needs a
// platform-specific retry that neither should have to remember.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// renameAttempts bounds the retry loop around the final rename.
const renameAttempts = 10

// renameBackoff is multiplied by the attempt number to space retries out
// over roughly a quarter of a second in total.
const renameBackoff = 5 * time.Millisecond

// WriteFile writes data to path atomically: the bytes go to a temporary
// file in the same directory, are flushed, and the temporary file is then
// renamed over the destination.
//
// perm is applied to the temporary file before the rename, so the
// destination never exists with wider permissions. On Windows the mode has
// no effect (ACLs govern access there).
//
// The parent directory is created when missing.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		// Remove the temporary file on every non-committed exit so a
		// failed write leaves no residue.
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Flush before the rename: without it a crash can leave a zero-length
	// or partially written file at the destination.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	// CreateTemp already uses 0600; this keeps a pre-existing umask or a
	// platform default from widening it.
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := replace(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	committed = true
	return nil
}

// replace renames tmpName over path, retrying a transient failure.
//
// On Windows a rename over an existing file can fail with "Access is
// denied" while another process holds a handle: a virus scanner, the
// search indexer, or a backup agent opens a freshly written file for
// inspection, and the sharing violation is momentary. A single os.Rename
// therefore turns a routine config save into a spurious error. Retrying
// briefly is the standard remedy; a permanent failure (a read-only
// directory, a directory in the way) still surfaces after the last
// attempt, unchanged.
func replace(tmpName, path string) error {
	var err error
	for attempt := 1; attempt <= renameAttempts; attempt++ {
		if err = os.Rename(tmpName, path); err == nil {
			return nil
		}
		if attempt < renameAttempts {
			time.Sleep(time.Duration(attempt) * renameBackoff)
		}
	}
	return err
}
