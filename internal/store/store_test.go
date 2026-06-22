package store

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore returns a Store configured with CAS path transform
// and a temp root, ready for use in tests.
func newTestStore() *Store {
	return NewStore(StoreOpts{
		Root:              "test_store_data",
		PathTransformFunc: CASPathTransformFunc,
	})
}

// teardownStore removes the entire storage root at the end of a test.
func teardownStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Clear(); err != nil {
		t.Errorf("teardown: failed to clear store: %v", err)
	}
}

func TestCASPathTransformFunc(t *testing.T) {
	key := "momsbestpicture"
	pathKey := CASPathTransformFunc(key)

	expectedFilename := "6804429f74181a63c50c3d81d733a12f14a353ff"
	expectedPathName := "68044/29f74/181a6/3c50c/3d81d/733a1/2f14a/353ff"

	assert.Equal(t, expectedPathName, pathKey.PathName,
		"CAS path name should match expected SHA-1-derived directory tree")
	assert.Equal(t, expectedFilename, pathKey.Filename,
		"CAS filename should be the full 40-char SHA-1 hex string")
}

func TestStoreWriteAndRead(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-alpha"
	const key = "my_test_file"
	data := []byte("hello from weaveFS")

	// Write
	n, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err, "Write should not return an error")
	assert.Equal(t, int64(len(data)), n, "Write should return correct byte count")

	// Read back
	size, rc, err := s.Read(nodeID, key)
	require.NoError(t, err, "Read should not return an error")
	defer rc.Close()

	assert.Equal(t, int64(len(data)), size, "Read should return correct file size")

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, data, got, "Read content should match written content")
}

func TestStoreHas(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-beta"
	const key = "existence_check"

	assert.False(t, s.Has(nodeID, key), "Has should return false before any write")

	_, err := s.Write(nodeID, key, bytes.NewReader([]byte("data")))
	require.NoError(t, err)

	assert.True(t, s.Has(nodeID, key), "Has should return true after write")

	require.NoError(t, s.Delete(nodeID, key))
	assert.False(t, s.Has(nodeID, key), "Has should return false after delete")
}

func TestStoreDelete(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-gamma"
	const key = "delete_me"

	_, err := s.Write(nodeID, key, bytes.NewReader([]byte("ephemeral")))
	require.NoError(t, err)

	require.True(t, s.Has(nodeID, key), "file should exist before delete")

	err = s.Delete(nodeID, key)
	require.NoError(t, err, "Delete should not return an error")

	assert.False(t, s.Has(nodeID, key), "file should be gone after delete")
}

func TestWriteVersionCreatesIndex(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-version"
	const key = "versioned_file"

	_, err := s.WriteVersion(nodeID, key, "initial commit", bytes.NewReader([]byte("hello v1")))
	require.NoError(t, err)

	_, err = s.WriteVersion(nodeID, key, "second commit", bytes.NewReader([]byte("hello v2")))
	require.NoError(t, err)

	versions, err := s.ListVersions(nodeID, key)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, 1, versions[0].Seq)
	assert.Equal(t, 2, versions[1].Seq)
	assert.Equal(t, "initial commit", versions[0].Message)
	assert.Equal(t, "second commit", versions[1].Message)
}

func TestReadSpecificVersion(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-version-read"
	const key = "read_versioned"

	v1, err := s.WriteVersion(nodeID, key, "", bytes.NewReader([]byte("content v1")))
	require.NoError(t, err)

	_, err = s.WriteVersion(nodeID, key, "", bytes.NewReader([]byte("content v2")))
	require.NoError(t, err)

	// Explicitly request v1 — should return original content even though v2 exists.
	_, rc, err := s.ReadVersion(nodeID, key, v1.VersionID)
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, []byte("content v1"), got)
}

func TestReadLatestVersion(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-version-latest"
	const key = "latest_versioned"

	_, err := s.WriteVersion(nodeID, key, "", bytes.NewReader([]byte("old")))
	require.NoError(t, err)
	_, err = s.WriteVersion(nodeID, key, "", bytes.NewReader([]byte("new")))
	require.NoError(t, err)

	_, rc, err := s.ReadVersion(nodeID, key, "")
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), got)
}

func TestDeleteVersion(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-delete-version"
	const key = "del_versioned"

	v1, err := s.WriteVersion(nodeID, key, "", bytes.NewReader([]byte("v1 data")))
	require.NoError(t, err)

	_, err = s.WriteVersion(nodeID, key, "", bytes.NewReader([]byte("v2 data")))
	require.NoError(t, err)

	require.NoError(t, s.DeleteVersion(nodeID, key, v1.VersionID))

	versions, err := s.ListVersions(nodeID, key)
	require.NoError(t, err)
	require.Len(t, versions, 1, "only v2 should remain")
	assert.Equal(t, 2, versions[0].Seq)

	// v2 must still be readable as the latest.
	_, rc, err := s.ReadVersion(nodeID, key, "")
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, []byte("v2 data"), got)
}

func TestRollbackTo(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-rollback"
	const key = "rollback_file"

	v1, err := s.WriteVersion(nodeID, key, "original", bytes.NewReader([]byte("v1 content")))
	require.NoError(t, err)

	_, err = s.WriteVersion(nodeID, key, "change", bytes.NewReader([]byte("v2 content")))
	require.NoError(t, err)

	// Roll back to v1 — must create a new v3 entry, not destroy history.
	rolled, err := s.RollbackTo(nodeID, key, v1.VersionID)
	require.NoError(t, err)
	assert.Equal(t, 3, rolled.Seq, "rollback should create seq 3")

	_, rc, err := s.ReadVersion(nodeID, key, "")
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, []byte("v1 content"), got, "latest should now contain v1's content")

	versions, err := s.ListVersions(nodeID, key)
	require.NoError(t, err)
	assert.Len(t, versions, 3, "history should have 3 entries")
}

func TestBackwardCompatWrite(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	const nodeID = "node-compat"
	const key = "compat_file"
	data := []byte("backward compatible")

	n, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)

	size, rc, err := s.Read(nodeID, key)
	require.NoError(t, err)
	defer rc.Close()
	assert.Equal(t, int64(len(data)), size)
	got, _ := io.ReadAll(rc)
	assert.Equal(t, data, got)
}
