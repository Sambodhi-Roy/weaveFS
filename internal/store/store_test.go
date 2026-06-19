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
