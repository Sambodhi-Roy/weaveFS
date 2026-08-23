package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sambodhi-Roy/weaveFS/internal/crypto"
	"github.com/Sambodhi-Roy/weaveFS/internal/node"
	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

// testNode is one simulated machine: its own data directory, its own identity,
// its own encryption key, and a FileServer over the two.
type testNode struct {
	node   *node.Node
	store  *store.Store
	server *FileServer
	dir    string
}

// newTestNode starts a complete node in a temporary directory.
//
// Discovery is disabled throughout. Multicast is blocked in many CI runners and
// containers, so tests connect peers explicitly instead — a suite that waits on
// mDNS fails for reasons unrelated to the code under test.
func newTestNode(t *testing.T, cfg Config) *testNode {
	t.Helper()

	dir := t.TempDir()

	n, err := node.New(context.Background(), node.Config{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		DataDir:     dir,
		DisableMDNS: true,
	})
	require.NoError(t, err, "starting the libp2p host must succeed")
	t.Cleanup(func() { _ = n.Close() })

	key, err := crypto.LoadOrCreateKey(dir)
	require.NoError(t, err)
	cipher, err := crypto.NewAESCipher(key)
	require.NoError(t, err)

	st := store.NewStore(store.StoreOpts{
		Root:              dir,
		PathTransformFunc: store.CASPathTransformFunc,
		Cipher:            cipher,
	})

	cfg.Node = n
	cfg.Store = st

	srv, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Close() })

	return &testNode{node: n, store: st, server: srv, dir: dir}
}

// connect wires two nodes together explicitly and waits until each has the
// other in its peer list, so a test never races the connection setup.
func connect(t *testing.T, a, b *testNode) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.NoError(t, a.node.Host().Connect(ctx, b.node.AddrInfo()))

	require.Eventually(t, func() bool {
		return contains(a.node.Peers(), b.node.Host().ID()) &&
			contains(b.node.Peers(), a.node.Host().ID())
	}, 10*time.Second, 20*time.Millisecond, "the two nodes should see each other")
}

func contains(peers []peer.ID, want peer.ID) bool {
	for _, p := range peers {
		if p == want {
			return true
		}
	}
	return false
}

// blobsUnder returns every blob file stored beneath ownerID inside dir,
// skipping the .vindex directory that holds metadata rather than content.
func blobsUnder(t *testing.T, dir, ownerID string) [][]byte {
	t.Helper()

	var found [][]byte
	root := filepath.Join(dir, ownerID)

	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".vindex" {
				return fs.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = append(found, data)
		return nil
	})
	require.NoError(t, err)

	return found
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var payload = []byte("weaveFS: distributed storage, one node at a time")

func TestNewValidatesConfig(t *testing.T) {
	_, err := New(Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Node is required")

	_, err = New(Config{Node: &node.Node{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Store is required")

	_, err = New(Config{Node: &node.Node{}, Store: store.NewStore(store.StoreOpts{}), ReplicationFactor: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be negative")
}

// TestStoreReplicatesToPeer checks the blob actually lands on the other machine,
// filed under the origin's PeerID rather than the custodian's.
func TestStoreReplicatesToPeer(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{})
	connect(t, origin, custodian)

	result, err := origin.server.Store(testCtx(t), "report", bytes.NewReader(payload))
	require.NoError(t, err)
	entry := result.Entry
	assert.Equal(t, int64(len(payload)), entry.SizeBytes, "the entry records the plaintext size")
	assert.Equal(t, 1, result.PeersTried)
	assert.Equal(t, 1, result.PeersStored, "the one connected peer took the copy")
	assert.Empty(t, result.Failures)

	assert.True(t, custodian.store.Has(origin.server.ID(), "report"),
		"the custodian should hold the blob under the origin's namespace")

	versions, err := custodian.store.ListVersions(origin.server.ID(), "report")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, entry.VersionID, versions[0].VersionID,
		"both nodes must agree on what this version is called")
	assert.Equal(t, entry.SizeBytes, versions[0].SizeBytes,
		"the custodian records the plaintext size even though it never sees plaintext")
}

// TestCustodianCannotReadBlob is the test this whole design exists for. It
// looks at the bytes on the custodian's disk rather than trusting any API.
func TestCustodianCannotReadBlob(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{})
	connect(t, origin, custodian)

	_, err := origin.server.Store(testCtx(t), "confidential", bytes.NewReader(payload))
	require.NoError(t, err)

	blobs := blobsUnder(t, custodian.dir, origin.server.ID())
	require.NotEmpty(t, blobs, "the custodian should have written something to disk")

	for _, blob := range blobs {
		assert.True(t, bytes.HasPrefix(blob, []byte("WFS1")),
			"the replica keeps the origin's blob header")
		assert.NotContains(t, string(blob), string(payload),
			"a custodian must never hold readable plaintext")
	}

	// And it genuinely cannot decrypt it: the custodian's own cipher is keyed
	// differently, so reading through its normal path yields something other
	// than the original.
	_, rc, err := custodian.store.Read(origin.server.ID(), "confidential")
	require.NoError(t, err, "the header parses, since the format is the same")
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.NotEqual(t, payload, got,
		"decrypting under the wrong key must not recover the plaintext")
}

// TestGetFetchesFromPeer walks the whole story: store, lose the local copy,
// get it back from the custodian.
func TestGetFetchesFromPeer(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{})
	connect(t, origin, custodian)

	ctx := testCtx(t)

	result, err := origin.server.Store(ctx, "report", bytes.NewReader(payload))
	require.NoError(t, err)
	entry := result.Entry

	// The origin loses everything it had.
	require.NoError(t, origin.store.Delete(origin.server.ID(), "report"))
	require.False(t, origin.store.Has(origin.server.ID(), "report"))

	size, rc, err := origin.server.Get(ctx, "report")
	require.NoError(t, err, "the origin must be able to recover its file")
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)

	assert.Equal(t, int64(len(payload)), size)
	assert.Equal(t, payload, got, "the recovered content must match byte for byte")

	versions, err := origin.server.ListVersions("report")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, entry.VersionID, versions[0].VersionID,
		"the recovered version keeps its original identity")
	assert.Equal(t, entry.CreatedAt.UTC(), versions[0].CreatedAt.UTC(),
		"and its original timestamp")
}

// TestShareGivesRecipientAReadableOwnedCopy is the test file sharing exists for,
// and the mirror image of TestCustodianCannotReadBlob: after a share the
// recipient holds the file under its *own* namespace, can read it, and its copy
// on disk is encrypted under its own key rather than the sender's.
func TestShareGivesRecipientAReadableOwnedCopy(t *testing.T) {
	sender := newTestNode(t, Config{})
	recipient := newTestNode(t, Config{})

	ctx := testCtx(t)

	// Put the file into the sender's store *before* connecting, so no custodian
	// replica lands on the recipient — this test is about the share alone.
	_, err := sender.server.Store(ctx, "report", bytes.NewReader(payload))
	require.NoError(t, err)

	connect(t, sender, recipient)

	result, err := sender.server.Share(ctx, sender.node.Peers(), "report", "", "", "for you")
	require.NoError(t, err)
	assert.Equal(t, 1, result.PeersTried)
	assert.Equal(t, 1, result.PeersStored)
	require.Len(t, result.Placements, 1)
	assert.Equal(t, recipient.server.ID(), result.Placements[0].Peer.String())
	assert.Empty(t, result.Failures)

	// The file is filed under the recipient's OWN id, not the sender's — this is
	// a handover, not a custodian copy.
	require.True(t, recipient.store.Has(recipient.server.ID(), "report"),
		"the recipient owns the shared file under its own namespace")
	assert.False(t, recipient.store.Has(sender.server.ID(), "report"),
		"and nothing is filed under the sender's namespace")

	// The recipient can read it back as plaintext.
	size, rc, err := recipient.store.Read(recipient.server.ID(), "report")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), size)
	assert.Equal(t, payload, got, "the recipient reads the original bytes")

	// The note rode along as the version message.
	versions, err := recipient.store.ListVersions(recipient.server.ID(), "report")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, "for you", versions[0].Message)

	// On disk the recipient's copy is encrypted under the recipient's own key:
	// it carries the WFS1 header and does not contain the plaintext.
	blobs := blobsUnder(t, recipient.dir, recipient.server.ID())
	require.NotEmpty(t, blobs)
	for _, blob := range blobs {
		assert.True(t, bytes.HasPrefix(blob, []byte("WFS1")))
		assert.NotContains(t, string(blob), string(payload),
			"the stored copy is ciphertext, re-encrypted under the recipient's key")
	}
}

// TestShareToExistingKeyAppendsAVersion pins the intended collision behaviour:
// sharing under a name the recipient already owns adds a new version rather than
// replacing or erroring, and the shared file becomes the latest.
func TestShareToExistingKeyAppendsAVersion(t *testing.T) {
	sender := newTestNode(t, Config{})
	recipient := newTestNode(t, Config{})
	connect(t, sender, recipient)

	ctx := testCtx(t)

	// The recipient already owns "notes".
	_, err := recipient.server.Store(ctx, "notes", bytes.NewReader([]byte("my own notes")))
	require.NoError(t, err)

	// The sender shares a different file under the same name.
	_, err = sender.server.Store(ctx, "notes", bytes.NewReader(payload))
	require.NoError(t, err)
	result, err := sender.server.Share(ctx, sender.node.Peers(), "notes", "", "", "")
	require.NoError(t, err)
	require.Equal(t, 1, result.PeersStored)

	versions, err := recipient.store.ListVersions(recipient.server.ID(), "notes")
	require.NoError(t, err)
	require.Len(t, versions, 2, "the share appends a version, keeping the recipient's original")

	// The latest read returns the shared file, not the recipient's original.
	_, rc, err := recipient.store.Read(recipient.server.ID(), "notes")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, payload, got, "the shared file is now the latest version")
}

// TestShareOfMissingKeyFailsBeforeTouchingTheNetwork checks the source is
// validated up front, so a share of a key the sender does not hold is one clear
// error rather than a per-peer pile of them.
func TestShareOfMissingKeyFailsBeforeTouchingTheNetwork(t *testing.T) {
	sender := newTestNode(t, Config{})
	recipient := newTestNode(t, Config{})
	connect(t, sender, recipient)

	_, err := sender.server.Share(testCtx(t), sender.node.Peers(), "nope", "", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not stored on this node")
}

// TestShareIsRefusedByAuthorizer checks the AllowShare gate is honoured: a
// recipient that refuses shares keeps nothing.
func TestShareIsRefusedByAuthorizer(t *testing.T) {
	sender := newTestNode(t, Config{})
	recipient := newTestNode(t, Config{Authorizer: refuseAll{}})
	connect(t, sender, recipient)

	ctx := testCtx(t)
	_, err := sender.server.Store(ctx, "report", bytes.NewReader(payload))
	require.NoError(t, err)

	result, err := sender.server.Share(ctx, sender.node.Peers(), "report", "", "", "")
	require.NoError(t, err, "a refusal is recorded, not returned as the call's error")
	assert.Equal(t, 0, result.PeersStored)
	require.Len(t, result.Failures, 1)
	assert.False(t, recipient.store.Has(recipient.server.ID(), "report"),
		"a refused share leaves nothing on the recipient")
}

// TestResolveTargets covers the three ways a share is aimed.
func TestResolveTargets(t *testing.T) {
	a := newTestNode(t, Config{})
	b := newTestNode(t, Config{})
	connect(t, a, b)

	bID := b.node.Host().ID()

	t.Run("a specific connected peer", func(t *testing.T) {
		got, err := a.server.ResolveTargets([]peer.ID{bID}, 0, false)
		require.NoError(t, err)
		assert.Equal(t, []peer.ID{bID}, got)
	})

	t.Run("a specific peer that is not connected", func(t *testing.T) {
		other := newTestNode(t, Config{})
		_, err := a.server.ResolveTargets([]peer.ID{other.node.Host().ID()}, 0, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not connected")
	})

	t.Run("all connected peers", func(t *testing.T) {
		got, err := a.server.ResolveTargets(nil, 0, true)
		require.NoError(t, err)
		assert.Equal(t, []peer.ID{bID}, got)
	})

	t.Run("a count, capped at what is connected", func(t *testing.T) {
		got, err := a.server.ResolveTargets(nil, 5, false)
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("no target given", func(t *testing.T) {
		_, err := a.server.ResolveTargets(nil, 0, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no target given")
	})
}

// TestGetIsLocalWhenPresent checks the common case costs no network traffic:
// with no peers at all, a local hit still works.
func TestGetIsLocalWhenPresent(t *testing.T) {
	solo := newTestNode(t, Config{})
	ctx := testCtx(t)

	_, err := solo.server.Store(ctx, "local", bytes.NewReader(payload))
	require.NoError(t, err)

	require.Empty(t, solo.node.Peers(), "this test is only meaningful with no peers")

	size, rc, err := solo.server.Get(ctx, "local")
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), size)
	assert.Equal(t, payload, got)
}

// TestStoreWithNoPeersSucceeds pins the best-effort promise: replication having
// nowhere to go must not fail the user's write.
func TestStoreWithNoPeersSucceeds(t *testing.T) {
	solo := newTestNode(t, Config{})

	result, err := solo.server.Store(testCtx(t), "lonely", bytes.NewReader(payload))
	require.NoError(t, err, "a write must succeed even with nobody to replicate to")
	assert.NotEmpty(t, result.Entry.VersionID)
	assert.True(t, solo.store.Has(solo.server.ID(), "lonely"))

	// The write succeeded and reached nobody, and the result says so. This is
	// the distinction the caller could not previously make.
	assert.Zero(t, result.PeersTried, "there was nobody to try")
	assert.Zero(t, result.PeersStored)
	assert.Empty(t, result.Failures, "reaching nobody is not the same as failing")
}

// TestStoreReportsRefusingPeer checks the other half: a peer that is tried and
// says no must show up as a failure rather than vanishing into a log line.
func TestStoreReportsRefusingPeer(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{Authorizer: refuseAll{}})
	connect(t, origin, custodian)

	result, err := origin.server.Store(testCtx(t), "report", bytes.NewReader(payload))
	require.NoError(t, err, "a refused replica must not fail the local write")

	assert.True(t, origin.store.Has(origin.server.ID(), "report"), "the local copy still landed")
	assert.Equal(t, 1, result.PeersTried)
	assert.Zero(t, result.PeersStored)

	require.Len(t, result.Failures, 1, "the refusal must be reported, not swallowed")
	assert.Equal(t, custodian.server.ID(), result.Failures[0].Peer.String())
	assert.Error(t, result.Failures[0].Err)
}

func TestGetUnknownKeyErrors(t *testing.T) {
	t.Run("with no peers", func(t *testing.T) {
		solo := newTestNode(t, Config{})

		_, _, err := solo.server.Get(testCtx(t), "never_written")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no peers are connected")
	})

	t.Run("with a peer that does not have it", func(t *testing.T) {
		a := newTestNode(t, Config{})
		b := newTestNode(t, Config{})
		connect(t, a, b)

		_, _, err := a.server.Get(testCtx(t), "never_written")
		require.Error(t, err, "a key nobody holds must error rather than hang")
		assert.Contains(t, err.Error(), "none of the")
	})
}

// refuseAll rejects every operation, standing in for a real policy.
type refuseAll struct{}

func (refuseAll) AllowStore(peer.ID, string, string) error {
	return fmt.Errorf("not on my disk")
}
func (refuseAll) AllowGet(peer.ID, string, string) error {
	return fmt.Errorf("mind your own business")
}
func (refuseAll) AllowShare(peer.ID, string) error {
	return fmt.Errorf("not accepting shares")
}

// toggleAuthorizer can be switched from permissive to hostile while a node is
// running, so a test can let a blob land and then refuse to give it back.
// The flag is atomic because handlers run in their own goroutines.
type toggleAuthorizer struct{ refuse atomic.Bool }

func (t *toggleAuthorizer) AllowStore(peer.ID, string, string) error { return t.decide() }
func (t *toggleAuthorizer) AllowGet(peer.ID, string, string) error   { return t.decide() }
func (t *toggleAuthorizer) AllowShare(peer.ID, string) error         { return t.decide() }

func (t *toggleAuthorizer) decide() error {
	if t.refuse.Load() {
		return fmt.Errorf("refused")
	}
	return nil
}

// TestAuthorizerIsHonoured checks the seam actually works, so that adding a
// real policy later is a matter of writing one rather than wiring one.
func TestAuthorizerIsHonoured(t *testing.T) {
	t.Run("a refusing peer stores nothing", func(t *testing.T) {
		origin := newTestNode(t, Config{})
		custodian := newTestNode(t, Config{Authorizer: refuseAll{}})
		connect(t, origin, custodian)

		// The local write still succeeds; replication is best effort.
		_, err := origin.server.Store(testCtx(t), "report", bytes.NewReader(payload))
		require.NoError(t, err)

		assert.False(t, custodian.store.Has(origin.server.ID(), "report"),
			"a refusing custodian must not have stored the blob")
	})

	t.Run("a refusing peer serves nothing", func(t *testing.T) {
		// The custodian accepts the blob and only then turns hostile, so the
		// test exercises a refused read rather than a missing file.
		policy := &toggleAuthorizer{}

		origin := newTestNode(t, Config{})
		custodian := newTestNode(t, Config{Authorizer: policy})
		connect(t, origin, custodian)

		ctx := testCtx(t)
		_, err := origin.server.Store(ctx, "report", bytes.NewReader(payload))
		require.NoError(t, err)
		require.True(t, custodian.store.Has(origin.server.ID(), "report"))

		policy.refuse.Store(true)
		require.NoError(t, origin.store.Delete(origin.server.ID(), "report"))

		_, _, err = origin.server.Get(ctx, "report")
		require.Error(t, err, "a refused fetch must not succeed")
	})
}

// TestReplicationFactorCapsCopies checks the knob is real rather than
// decorative.
func TestReplicationFactorCapsCopies(t *testing.T) {
	origin := newTestNode(t, Config{ReplicationFactor: 1})
	first := newTestNode(t, Config{})
	second := newTestNode(t, Config{})

	connect(t, origin, first)
	connect(t, origin, second)

	_, err := origin.server.Store(testCtx(t), "report", bytes.NewReader(payload))
	require.NoError(t, err)

	copies := 0
	for _, peerNode := range []*testNode{first, second} {
		if peerNode.store.Has(origin.server.ID(), "report") {
			copies++
		}
	}
	assert.Equal(t, 1, copies, "ReplicationFactor 1 must produce exactly one replica")
}

// TestLargeFileSurvivesTheRoundTrip exercises a payload far larger than any
// single read or write, which is where a naive copy loop or a bad size
// calculation would show up.
func TestLargeFileSurvivesTheRoundTrip(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{})
	connect(t, origin, custodian)

	// Not random: a repeating pattern makes a truncation or an offset error
	// obvious when a comparison fails.
	large := bytes.Repeat([]byte("weaveFS-0123456789-"), 60_000) // ~1.1 MB
	ctx := testCtx(t)

	_, err := origin.server.Store(ctx, "big", bytes.NewReader(large))
	require.NoError(t, err)

	require.NoError(t, origin.store.Delete(origin.server.ID(), "big"))

	size, rc, err := origin.server.Get(ctx, "big")
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)

	assert.Equal(t, int64(len(large)), size)
	assert.True(t, bytes.Equal(large, got), "a multi-megabyte file must survive intact")
}

// TestConcurrentStoresDoNotInterfere runs several writes at once, since libp2p
// serves every incoming stream in its own goroutine and this is the first
// concurrent code in the project.
//
// Note the race detector cannot run on the current development machine, so this
// checks outcomes rather than proving the absence of races.
func TestConcurrentStoresDoNotInterfere(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{})
	connect(t, origin, custodian)

	ctx := testCtx(t)
	const writers = 8

	errs := make(chan error, writers)
	for i := range writers {
		go func() {
			key := fmt.Sprintf("file-%d", i)
			body := bytes.Repeat([]byte(key+" "), 500)

			_, err := origin.server.Store(ctx, key, bytes.NewReader(body))
			errs <- err
		}()
	}

	for range writers {
		require.NoError(t, <-errs)
	}

	for i := range writers {
		key := fmt.Sprintf("file-%d", i)
		want := bytes.Repeat([]byte(key+" "), 500)

		require.True(t, custodian.store.Has(origin.server.ID(), key),
			"%s should have been replicated", key)

		size, rc, err := origin.server.Get(ctx, key)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())

		assert.Equal(t, int64(len(want)), size)
		assert.True(t, bytes.Equal(want, got), "%s must not have been mixed up with another", key)
	}
}

// TestMultipleVersionsReplicate checks the version history survives the trip,
// rather than only the newest write.
func TestMultipleVersionsReplicate(t *testing.T) {
	origin := newTestNode(t, Config{})
	custodian := newTestNode(t, Config{})
	connect(t, origin, custodian)

	ctx := testCtx(t)

	first, err := origin.server.StoreVersion(ctx, "doc", "first draft", bytes.NewReader([]byte("version one")))
	require.NoError(t, err)
	second, err := origin.server.StoreVersion(ctx, "doc", "second draft", bytes.NewReader([]byte("version two")))
	require.NoError(t, err)

	versions, err := custodian.store.ListVersions(origin.server.ID(), "doc")
	require.NoError(t, err)
	require.Len(t, versions, 2, "the custodian should hold both versions")

	assert.Equal(t, first.Entry.VersionID, versions[0].VersionID)
	assert.Equal(t, second.Entry.VersionID, versions[1].VersionID)
	assert.Equal(t, "first draft", versions[0].Message, "messages travel with the version")
	assert.Equal(t, 1, versions[0].Seq, "and so does the origin's sequence number")
	assert.Equal(t, 2, versions[1].Seq)
}
