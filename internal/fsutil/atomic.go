// Package fsutil holds small filesystem helpers shared across weaveFS packages.
//
// It exists to keep one copy of operations that several packages need to get
// exactly right — currently just atomic file replacement, which both the node
// identity key and the blob encryption key depend on.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path so that a crash can never leave a
// partially written file in place.
//
// It writes to a temporary sibling first and then renames it over the target.
// Renaming within a single filesystem is atomic: the operating system
// guarantees observers see either the old file or the complete new one, never
// a half-written mix. A crash can therefore leave a stray ".tmp" file behind,
// but it cannot leave a truncated key or index that fails to parse on the next
// startup.
//
// The temporary file is deliberately a sibling of the target rather than
// somewhere under the system temp directory, because os.Rename cannot be
// atomic — and on Windows will fail outright — across filesystem boundaries.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return fmt.Errorf("fsutil: creating directory for %s: %w", path, err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("fsutil: writing temp file %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the write already failed
		return fmt.Errorf("fsutil: renaming %s to %s: %w", tmp, path, err)
	}
	return nil
}
