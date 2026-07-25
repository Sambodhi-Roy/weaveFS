package store

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sambodhi-Roy/weaveFS/internal/crypto"
)

// Compile-time proof that the real cipher satisfies the interface this package
// declares. Go checks interface satisfaction implicitly, so without an
// assertion like this a mismatch would only surface at the call site.
var _ Cipher = (*crypto.AESCipher)(nil)

// newEncryptedTestStore mirrors newTestStore but configures a real AES cipher,
// so these tests exercise the actual encryption path rather than a stand-in.
func newEncryptedTestStore(t *testing.T) *Store {
	t.Helper()

	key := make([]byte, crypto.KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	cipher, err := crypto.NewAESCipher(key)
	require.NoError(t, err, "test cipher must build")

	return NewStore(StoreOpts{
		Root:              "test_store_data",
		PathTransformFunc: CASPathTransformFunc,
		Cipher:            cipher,
	})
}

// findBlobs returns every blob file stored for nodeID, skipping the .vindex
// directory that holds version metadata rather than content.
func findBlobs(t *testing.T, root, nodeID string) []string {
	t.Helper()

	var blobs []string
	nodeDir := filepath.Join(root, nodeID)

	err := filepath.WalkDir(nodeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".vindex" {
				return fs.SkipDir
			}
			return nil
		}
		blobs = append(blobs, path)
		return nil
	})
	require.NoError(t, err, "walking %s should succeed", nodeDir)

	return blobs
}

func TestEncryptedWriteAndRead(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	const nodeID = "node-encrypted"
	const key = "secret_document"
	data := []byte("the launch codes are hunter2")

	n, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err, "Write should succeed with a cipher configured")
	assert.Equal(t, int64(len(data)), n, "Write should report the plaintext byte count")

	size, rc, err := s.Read(nodeID, key)
	require.NoError(t, err, "Read should succeed")
	defer rc.Close()

	assert.Equal(t, int64(len(data)), size, "Read should report the plaintext size")

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, data, got, "content should survive the encrypt/decrypt round trip")
}

// TestEncryptedBlobOnDiskIsNotPlaintext is the test the whole segment exists
// for: it looks at the actual bytes on disk rather than trusting the API.
func TestEncryptedBlobOnDiskIsNotPlaintext(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	const nodeID = "node-ciphertext-on-disk"
	const key = "confidential"
	data := []byte("weaveFS: distributed storage, one node at a time")

	_, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err)

	blobs := findBlobs(t, s.Root, nodeID)
	require.Len(t, blobs, 1, "one write should produce exactly one blob")

	raw, err := os.ReadFile(blobs[0])
	require.NoError(t, err)

	assert.True(t, bytes.HasPrefix(raw, []byte("WFS1")),
		"the blob should start with the weaveFS encrypted-blob magic")
	assert.False(t, bytes.Contains(raw, data),
		"the plaintext must not appear anywhere in the stored blob")
	assert.False(t, strings.Contains(string(raw), "weaveFS: distributed"),
		"no readable fragment of the plaintext should survive")
}

// TestEncryptedSizeIsPlaintextSize pins decision 4 from both sides at once: the
// API reports plaintext length while the file on disk carries the overhead.
func TestEncryptedSizeIsPlaintextSize(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	const nodeID = "node-encrypted-size"
	const key = "measured_file"
	data := []byte("exactly this many bytes, please")

	written, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), written, "Write must report plaintext size")

	read, rc, err := s.Read(nodeID, key)
	require.NoError(t, err)
	defer rc.Close()
	assert.Equal(t, int64(len(data)), read, "Read must report plaintext size")

	blobs := findBlobs(t, s.Root, nodeID)
	require.Len(t, blobs, 1)

	info, err := os.Stat(blobs[0])
	require.NoError(t, err)
	assert.Equal(t, int64(len(data))+s.Cipher.Overhead(), info.Size(),
		"the file on disk should be exactly Overhead() bytes larger than the plaintext")
}

// TestEncryptedVersioningRoundTrip checks that the versioning layer above the
// cipher is entirely unaffected: old versions stay independently readable and
// rollback still works, each version having been encrypted under its own IV.
func TestEncryptedVersioningRoundTrip(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	const nodeID = "node-encrypted-versions"
	const key = "draft"
	first := []byte("first draft, full of typos")
	second := []byte("second draft, considerably better")

	v1, err := s.WriteVersion(nodeID, key, "initial", bytes.NewReader(first))
	require.NoError(t, err)
	v2, err := s.WriteVersion(nodeID, key, "revised", bytes.NewReader(second))
	require.NoError(t, err)

	// The older version must still decrypt correctly on its own.
	size, rc, err := s.ReadVersion(nodeID, key, v1.VersionID)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	assert.Equal(t, first, got, "version 1 should still be readable")
	assert.Equal(t, int64(len(first)), size)

	// The latest must be version 2.
	_, rc, err = s.Read(nodeID, key)
	require.NoError(t, err)
	got, err = io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	assert.Equal(t, second, got, "the latest version should be version 2")

	// Rolling back re-encrypts version 1's content as a brand new version.
	v3, err := s.RollbackTo(nodeID, key, v1.VersionID)
	require.NoError(t, err)
	assert.Equal(t, 3, v3.Seq, "rollback should append rather than rewrite history")
	assert.NotEqual(t, v2.VersionID, v3.VersionID)

	_, rc, err = s.Read(nodeID, key)
	require.NoError(t, err)
	got, err = io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	assert.Equal(t, first, got, "after rollback the latest content should be version 1's")

	versions, err := s.ListVersions(nodeID, key)
	require.NoError(t, err)
	assert.Len(t, versions, 3, "history should hold all three versions")
}

// TestEncryptedBlobsUseDistinctIVs writes identical content under two keys and
// confirms the stored bytes differ. Identical ciphertext would mean a reused
// IV, which is the one failure mode CTR mode cannot survive.
func TestEncryptedBlobsUseDistinctIVs(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	const nodeID = "node-encrypted-ivs"
	data := []byte("identical content stored twice")

	_, err := s.Write(nodeID, "copy_a", bytes.NewReader(data))
	require.NoError(t, err)
	_, err = s.Write(nodeID, "copy_b", bytes.NewReader(data))
	require.NoError(t, err)

	blobs := findBlobs(t, s.Root, nodeID)
	require.Len(t, blobs, 2, "two keys should produce two blobs")

	a, err := os.ReadFile(blobs[0])
	require.NoError(t, err)
	b, err := os.ReadFile(blobs[1])
	require.NoError(t, err)

	assert.NotEqual(t, a, b,
		"identical plaintext must not produce identical ciphertext")
}

// TestNilCipherStoresPlaintext is the backward-compatibility guard: the default
// StoreOpts must behave exactly as it did before this feature existed.
func TestNilCipherStoresPlaintext(t *testing.T) {
	s := newTestStore()
	defer teardownStore(t, s)

	require.Nil(t, s.Cipher, "the default StoreOpts must not configure a cipher")

	const nodeID = "node-plaintext-default"
	const key = "readable"
	data := []byte("this one is deliberately not encrypted")

	_, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err)

	blobs := findBlobs(t, s.Root, nodeID)
	require.Len(t, blobs, 1)

	raw, err := os.ReadFile(blobs[0])
	require.NoError(t, err)
	assert.Equal(t, data, raw, "without a cipher the blob should be stored verbatim")
}

// TestEncryptedStoreRejectsPlaintextBlob covers the upgrade hazard: a data
// directory written before encryption was enabled must fail loudly rather than
// hand back CTR-mangled garbage as though the read had succeeded.
func TestEncryptedStoreRejectsPlaintextBlob(t *testing.T) {
	const nodeID = "node-mixed-mode"
	const key = "written_before_encryption"
	data := []byte("stored back when blobs were plaintext")

	// Write with no cipher...
	plain := newTestStore()
	defer teardownStore(t, plain)
	_, err := plain.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err)

	// ...then read through a store that expects ciphertext.
	encrypted := newEncryptedTestStore(t)
	_, _, err = encrypted.Read(nodeID, key)

	require.Error(t, err, "a plaintext blob must not be silently decrypted to garbage")
	assert.Contains(t, err.Error(), "not a weaveFS encrypted blob")
}
