package store

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sambodhi-Roy/weaveFS/internal/crypto"
)

// custodianRoot is a second storage root, standing in for a second machine.
// The custodian stores blobs under the *origin's* node ID, so origin and
// custodian cannot share a root without colliding on identical paths.
const custodianRoot = "test_store_data_custodian"

// newCustodianStore returns a store with no cipher at all, which is the honest
// model of a peer holding someone else's data: it has no key, so it can only
// move bytes around, never read them.
func newCustodianStore() *Store {
	return NewStore(StoreOpts{
		Root:              custodianRoot,
		PathTransformFunc: CASPathTransformFunc,
	})
}

// replicate performs the whole custodian handoff: read the blob out of one
// store exactly as stored, and write those same bytes into another. This is
// what internal/server does over a network stream, with the network removed.
func replicate(t *testing.T, from, to *Store, nodeID, key, versionID string) int64 {
	t.Helper()

	blobSize, rc, err := from.ReadVersionRaw(nodeID, key, versionID)
	require.NoError(t, err, "reading the raw blob must succeed")
	defer rc.Close()

	versions, err := from.ListVersions(nodeID, key)
	require.NoError(t, err)
	require.NotEmpty(t, versions)

	meta := versions[len(versions)-1]
	require.NoError(t, to.WriteVersionRaw(nodeID, key, meta, rc),
		"the custodian must accept the raw blob")

	return blobSize
}

func TestReadVersionRawReturnsCiphertext(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	const nodeID = "node-raw-ciphertext"
	const key = "confidential"
	data := []byte("weaveFS: distributed storage, one node at a time")

	_, err := s.Write(nodeID, key, bytes.NewReader(data))
	require.NoError(t, err)

	_, rc, err := s.ReadVersionRaw(nodeID, key, "")
	require.NoError(t, err, "ReadVersionRaw should succeed")
	defer rc.Close()

	raw, err := io.ReadAll(rc)
	require.NoError(t, err)

	assert.True(t, bytes.HasPrefix(raw, []byte("WFS1")),
		"the raw blob must still carry the cipher's header, got % x", raw[:4])
	assert.NotContains(t, string(raw), string(data),
		"ReadVersionRaw must not decrypt — that is the entire point of it")
}

// TestReadVersionRawSizeIsOnDiskSize pins the deliberate difference between
// this function and every other read in the package: it reports what is
// physically on disk, because that is what a caller is about to copy.
func TestReadVersionRawSizeIsOnDiskSize(t *testing.T) {
	data := []byte("size accounting matters here")

	t.Run("with a cipher the raw size includes the header", func(t *testing.T) {
		s := newEncryptedTestStore(t)
		defer teardownStore(t, s)

		_, err := s.Write("node-sized", "k", bytes.NewReader(data))
		require.NoError(t, err)

		plaintextSize, rc, err := s.Read("node-sized", "k")
		require.NoError(t, err)
		require.NoError(t, rc.Close())

		rawSize, rrc, err := s.ReadVersionRaw("node-sized", "k", "")
		require.NoError(t, err)
		require.NoError(t, rrc.Close())

		assert.Equal(t, int64(len(data)), plaintextSize,
			"Read reports the plaintext length")
		assert.Equal(t, plaintextSize+s.Cipher.Overhead(), rawSize,
			"ReadVersionRaw reports the on-disk length, which is larger by the header")
	})

	t.Run("without a cipher the two agree", func(t *testing.T) {
		s := newTestStore()
		defer teardownStore(t, s)

		_, err := s.Write("node-sized", "k", bytes.NewReader(data))
		require.NoError(t, err)

		rawSize, rc, err := s.ReadVersionRaw("node-sized", "k", "")
		require.NoError(t, err)
		require.NoError(t, rc.Close())

		assert.Equal(t, int64(len(data)), rawSize,
			"with no cipher there is no overhead, so the sizes match")
	})
}

func TestReadVersionRawUnknownKeyErrors(t *testing.T) {
	s := newEncryptedTestStore(t)
	defer teardownStore(t, s)

	_, _, err := s.ReadVersionRaw("node-missing", "never_written", "")
	require.Error(t, err, "a key with no versions must be an error, not an empty reader")
}

// TestRawRoundTripThroughCustodian is the test this file exists for. It walks
// the complete replication story: the origin stores a file, hands the raw bytes
// to a custodian that has no key, the origin loses its own copy, and then gets
// the file back from the custodian and reads it as plaintext.
func TestRawRoundTripThroughCustodian(t *testing.T) {
	origin := newEncryptedTestStore(t)
	defer teardownStore(t, origin)

	custodian := newCustodianStore()
	defer teardownStore(t, custodian)

	const nodeID = "origin-node"
	const key = "quarterly_report"
	data := []byte("weaveFS: distributed storage, one node at a time")

	entry, err := origin.WriteVersion(nodeID, key, "first draft", bytes.NewReader(data))
	require.NoError(t, err)

	// Replicate to the custodian, which stores it under the origin's node ID.
	replicate(t, origin, custodian, nodeID, key, "")

	// The custodian holds the bytes but cannot make sense of them.
	custodianSize, crc, err := custodian.ReadVersionRaw(nodeID, key, "")
	require.NoError(t, err, "the custodian must be able to read back what it stored")
	custodianBytes, err := io.ReadAll(crc)
	require.NoError(t, err)
	require.NoError(t, crc.Close())

	assert.Equal(t, int64(len(data))+origin.Cipher.Overhead(), custodianSize)
	assert.NotContains(t, string(custodianBytes), string(data),
		"a custodian must never hold readable plaintext")

	// The origin loses everything.
	require.NoError(t, origin.Delete(nodeID, key))
	require.False(t, origin.Has(nodeID, key), "the origin should have nothing left")

	// Fetch it back from the custodian and write it down verbatim.
	replicate(t, custodian, origin, nodeID, key, "")

	// Because the bytes are the origin's own ciphertext, the ordinary
	// decrypting read path just works.
	size, rc, err := origin.Read(nodeID, key)
	require.NoError(t, err, "the origin must be able to read its recovered file")
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)

	assert.Equal(t, int64(len(data)), size, "the recovered size must be the plaintext size")
	assert.Equal(t, data, got, "the recovered content must match byte for byte")
	assert.Equal(t, entry.VersionID, mustLatest(t, origin, nodeID, key),
		"the recovered version must keep its original identity")
}

// TestWriteVersionRawPreservesMeta checks that a custodian records the
// origin's version identity rather than inventing its own. Without this, the
// origin could never ask for one specific version by ID.
func TestWriteVersionRawPreservesMeta(t *testing.T) {
	custodian := newCustodianStore()
	defer teardownStore(t, custodian)

	meta := VersionEntry{
		VersionID: "11111111-2222-4333-8444-555555555555",
		Seq:       7,
		CreatedAt: time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
		SizeBytes: 42,
		Message:   "written on another machine",
	}

	require.NoError(t, custodian.WriteVersionRaw("far-away-node", "k", meta,
		bytes.NewReader([]byte("some opaque bytes"))))

	versions, err := custodian.ListVersions("far-away-node", "k")
	require.NoError(t, err)
	require.Len(t, versions, 1)

	assert.Equal(t, meta, versions[0],
		"every field of the origin's entry must survive unchanged, including "+
			"SizeBytes, which stays the plaintext length the custodian never sees")
}

func TestWriteVersionRawUpdatesLatest(t *testing.T) {
	custodian := newCustodianStore()
	defer teardownStore(t, custodian)

	const nodeID = "far-away-node"
	const key = "k"

	older := VersionEntry{VersionID: "aaaa", Seq: 1, SizeBytes: 3}
	newer := VersionEntry{VersionID: "bbbb", Seq: 2, SizeBytes: 3}

	require.NoError(t, custodian.WriteVersionRaw(nodeID, key, older, bytes.NewReader([]byte("one"))))
	require.NoError(t, custodian.WriteVersionRaw(nodeID, key, newer, bytes.NewReader([]byte("two"))))

	assert.True(t, custodian.Has(nodeID, key))
	assert.Equal(t, newer.VersionID, mustLatest(t, custodian, nodeID, key),
		"the most recently written version must become the latest")

	// An empty versionID must resolve to that same latest version.
	_, rc, err := custodian.ReadVersionRaw(nodeID, key, "")
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, []byte("two"), got)
}

// TestWriteVersionRawIsIdempotent covers a retried replication attempt: the
// same version arriving twice must converge, not accumulate duplicates.
func TestWriteVersionRawIsIdempotent(t *testing.T) {
	custodian := newCustodianStore()
	defer teardownStore(t, custodian)

	meta := VersionEntry{VersionID: "cccc", Seq: 1, SizeBytes: 5}

	require.NoError(t, custodian.WriteVersionRaw("n", "k", meta, bytes.NewReader([]byte("hello"))))
	require.NoError(t, custodian.WriteVersionRaw("n", "k", meta, bytes.NewReader([]byte("hello"))))

	versions, err := custodian.ListVersions("n", "k")
	require.NoError(t, err)
	assert.Len(t, versions, 1, "re-storing a known version must replace it, not duplicate it")
}

func TestWriteVersionRawRequiresVersionID(t *testing.T) {
	custodian := newCustodianStore()
	defer teardownStore(t, custodian)

	err := custodian.WriteVersionRaw("n", "k", VersionEntry{Seq: 1}, bytes.NewReader(nil))

	require.Error(t, err, "an entry with no VersionID must be rejected")
	assert.Contains(t, err.Error(), "VersionID is required")
}

// TestWriteVersionRawHonoursMaxVersions checks a custodian applies the same
// retention limit as an origin, so it cannot grow without bound.
func TestWriteVersionRawHonoursMaxVersions(t *testing.T) {
	custodian := NewStore(StoreOpts{
		Root:              custodianRoot,
		PathTransformFunc: CASPathTransformFunc,
		MaxVersions:       2,
	})
	defer teardownStore(t, custodian)

	for _, id := range []string{"v1", "v2", "v3"} {
		meta := VersionEntry{VersionID: id, SizeBytes: 1}
		require.NoError(t, custodian.WriteVersionRaw("n", "k", meta, bytes.NewReader([]byte("x"))))
	}

	versions, err := custodian.ListVersions("n", "k")
	require.NoError(t, err)
	require.Len(t, versions, 2, "MaxVersions must prune the oldest entries")
	assert.Equal(t, "v2", versions[0].VersionID)
	assert.Equal(t, "v3", versions[1].VersionID)
}

// TestRawPathsWorkWithoutACipher confirms the raw functions are not
// encryption-specific: with no cipher configured they simply copy bytes.
func TestRawPathsWorkWithoutACipher(t *testing.T) {
	origin := newTestStore()
	defer teardownStore(t, origin)

	custodian := newCustodianStore()
	defer teardownStore(t, custodian)

	data := []byte("plaintext all the way down")
	_, err := origin.Write("n", "k", bytes.NewReader(data))
	require.NoError(t, err)

	replicate(t, origin, custodian, "n", "k", "")

	size, rc, err := custodian.Read("n", "k")
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)
	assert.Equal(t, data, got)
}

// mustLatest returns the VersionID the store currently considers latest.
func mustLatest(t *testing.T, s *Store, nodeID, key string) string {
	t.Helper()

	versions, err := s.ListVersions(nodeID, key)
	require.NoError(t, err)
	require.NotEmpty(t, versions, "expected at least one version")

	return versions[len(versions)-1].VersionID
}

// Compile-time proof that a real cipher still satisfies the interface after the
// raw paths were added, since raw.go deliberately bypasses it.
var _ Cipher = (*crypto.AESCipher)(nil)
