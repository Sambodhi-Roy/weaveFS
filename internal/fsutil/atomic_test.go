package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFileAtomicCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")
	want := []byte("written atomically")

	require.NoError(t, WriteFileAtomic(path, want, 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")

	require.NoError(t, WriteFileAtomic(path, []byte("first"), 0o600))
	require.NoError(t, WriteFileAtomic(path, []byte("second"), 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("second"), got,
		"a second write must replace the first, not fail or append")
}

func TestWriteFileAtomicCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "deep.txt")

	require.NoError(t, WriteFileAtomic(path, []byte("nested"), 0o600),
		"missing parent directories should be created, not reported as an error")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("nested"), got)
}

// TestWriteFileAtomicLeavesNoTempFile checks the cleanup half of the contract:
// after a successful write the temporary sibling must be gone, because it was
// renamed rather than copied.
func TestWriteFileAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")

	require.NoError(t, WriteFileAtomic(path, []byte("data"), 0o600))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the target file should remain")
	assert.Equal(t, "hello.txt", entries[0].Name())
}

func TestWriteFileAtomicAppliesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	path := filepath.Join(t.TempDir(), "secret.key")
	require.NoError(t, WriteFileAtomic(path, []byte("sensitive"), 0o600))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"the mode must survive the write-then-rename")
}

func TestWriteFileAtomicErrorsOnUnwritablePath(t *testing.T) {
	// An existing regular file cannot also be a parent directory, so this
	// exercises the MkdirAll failure branch on every platform.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	err := WriteFileAtomic(filepath.Join(blocker, "child.txt"), []byte("data"), 0o600)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fsutil:", "errors should be package-prefixed")
}
