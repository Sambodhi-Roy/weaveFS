package node

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackAddr keeps every test off the wider network. Port 0 asks the OS for a
// free port, which prevents the "address already in use" flakes that a fixed
// port causes when tests run in parallel or in quick succession.
const loopbackAddr = "/ip4/127.0.0.1/tcp/0"

// newTestNode starts a node in a temporary directory with discovery switched
// off, and registers its shutdown with the test.
//
// Discovery is off by default on purpose. Multicast is routinely blocked in CI
// containers and sandboxes, so a test that waits on mDNS fails for reasons that
// have nothing to do with the code under test. Exactly one test below exercises
// real discovery, and it is skipped under -short.
func newTestNode(t *testing.T, dataDir string) *Node {
	t.Helper()

	n, err := New(context.Background(), Config{
		ListenAddrs: []string{loopbackAddr},
		DataDir:     dataDir,
		DisableMDNS: true,
	})
	require.NoError(t, err, "starting a test node must succeed")

	t.Cleanup(func() { _ = n.Close() })
	return n
}

// TestIdentityPersistsAcrossRestart is the most important test in this file.
//
// internal/store scopes every stored path by the node's PeerID, and the PeerID
// is derived from the private key in <DataDir>/identity.key. If that key were
// ever regenerated on startup instead of loaded, the store would begin writing
// to a brand-new directory and every previously stored file would be orphaned
// on disk — no error, no warning, the node simply appearing empty.
func TestIdentityPersistsAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()

	first := newTestNode(t, dataDir)
	firstID := first.ID()
	require.NoError(t, first.Close(), "closing the first node must succeed")

	second := newTestNode(t, dataDir)

	assert.Equal(t, firstID, second.ID(),
		"a node restarted against the same DataDir must keep its PeerID, "+
			"otherwise every previously stored file is silently orphaned")
}

// TestIdentityFileIsPrivate checks the key is not world-readable.
//
// Skipped on Windows, where Unix permission bits are not enforced: the file
// would report whatever mode it was created with regardless of what the OS
// actually allows, so a pass there would prove nothing.
func TestIdentityFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	dataDir := t.TempDir()
	newTestNode(t, dataDir)

	info, err := os.Stat(filepath.Join(dataDir, identityFileName))
	require.NoError(t, err, "identity.key must exist after the node starts")

	assert.Equal(t, os.FileMode(identityFileMode), info.Mode().Perm(),
		"identity.key is a real private key and must not be readable by other users")
}

// TestTwoNodesConnect verifies two nodes can find each other's addresses and
// establish a connection, without involving discovery at all.
func TestTwoNodesConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a := newTestNode(t, t.TempDir())
	b := newTestNode(t, t.TempDir())

	require.NoError(t, a.Host().Connect(ctx, b.AddrInfo()),
		"connecting to a peer by its AddrInfo must succeed")

	assert.Contains(t, a.Peers(), b.Host().ID(), "a should see b as a connected peer")
	assert.Contains(t, b.Peers(), a.Host().ID(), "b should see a as a connected peer")
}

// TestNodeAddrsAreReal asserts Addrs reports the port the OS actually assigned.
//
// This is the direct replacement for a bug in the deleted hand-written
// transport, whose Addr() returned the literal configured string ":0" rather
// than the bound address — so nothing could ever dial it.
func TestNodeAddrsAreReal(t *testing.T) {
	n := newTestNode(t, t.TempDir())

	addrs := n.Addrs()
	require.NotEmpty(t, addrs, "a listening node must report at least one address")

	for _, addr := range addrs {
		assert.NotContains(t, addr, "/tcp/0",
			"Addrs must report the port the OS assigned, not the port 0 we asked for")
		assert.Contains(t, addr, "/p2p/",
			"each address must carry the PeerID so another node can dial it directly")
	}
}

// TestCloseIsIdempotent covers the case where a caller closes on an error path
// and again from a defer. The old TCPTransport.Close panicked when it had never
// been started; Node guards against that with a sync.Once.
func TestCloseIsIdempotent(t *testing.T) {
	n := newTestNode(t, t.TempDir())

	require.NoError(t, n.Close(), "the first Close must succeed")
	assert.NotPanics(t, func() { _ = n.Close() }, "a second Close must not panic")
}

// TestConfigDefaults checks the zero-value config is filled in sensibly, since
// callers are expected to set only DataDir in the common case.
func TestConfigDefaults(t *testing.T) {
	got := Config{DataDir: "somewhere"}.withDefaults()

	assert.Equal(t, []string{defaultListenAddr}, got.ListenAddrs,
		"an empty ListenAddrs should become the all-interfaces default")
	assert.Equal(t, defaultServiceName, got.ServiceName,
		"an empty ServiceName should become the weavefs discovery tag")
}

// TestConfigDefaultsLeaveExplicitValuesAlone is the other half of the contract:
// defaults must fill gaps, never override a caller's choice.
func TestConfigDefaultsLeaveExplicitValuesAlone(t *testing.T) {
	got := Config{
		ListenAddrs: []string{loopbackAddr},
		DataDir:     "somewhere",
		ServiceName: "custom-network",
	}.withDefaults()

	assert.Equal(t, []string{loopbackAddr}, got.ListenAddrs)
	assert.Equal(t, "custom-network", got.ServiceName)
}

// TestNewRequiresDataDir checks the one configuration mistake that cannot be
// defaulted away: without a directory there is nowhere to persist the identity,
// and a node with a fresh identity every run is worse than no node at all.
func TestNewRequiresDataDir(t *testing.T) {
	n, err := New(context.Background(), Config{ListenAddrs: []string{loopbackAddr}})

	require.Error(t, err, "New must reject an empty DataDir rather than pick one")
	assert.Nil(t, n, "no node should be returned alongside an error")
	assert.Contains(t, err.Error(), "DataDir is required")
}

// TestDiscoveryInactiveWhenDisabled checks DiscoveryActive reports the truth
// when discovery was switched off. Callers use it to decide whether they must
// connect peers explicitly.
func TestDiscoveryInactiveWhenDisabled(t *testing.T) {
	n := newTestNode(t, t.TempDir())

	assert.False(t, n.DiscoveryActive(),
		"DiscoveryActive must be false when DisableMDNS was set")
}

// TestCorruptIdentityFails checks that an unreadable key is an error rather
// than a silent regeneration.
//
// This is the failure mode that matters: quietly generating a replacement key
// would give the node a new PeerID, pointing internal/store at an empty
// directory and orphaning every stored file. Failing loudly lets the operator
// restore the file from a backup instead.
func TestCorruptIdentityFails(t *testing.T) {
	dataDir := t.TempDir()
	newTestNode(t, dataDir)

	path := filepath.Join(dataDir, identityFileName)
	require.NoError(t, os.WriteFile(path, []byte("this is not a private key"), identityFileMode))

	n, err := New(context.Background(), Config{
		ListenAddrs: []string{loopbackAddr},
		DataDir:     dataDir,
		DisableMDNS: true,
	})
	if n != nil {
		_ = n.Close()
	}

	require.Error(t, err, "a corrupt identity.key must fail loudly, never regenerate silently")
	assert.True(t, strings.Contains(err.Error(), "corrupt"),
		"the error should name the problem, got: %v", err)
}

// TestMDNSDiscovery is the only test that exercises real local-network
// discovery. It is skipped under -short because multicast is unavailable in
// many CI runners, container networks and sandboxes.
func TestMDNSDiscovery(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS needs multicast, which is unavailable in many environments")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A service name unique to this test keeps it from connecting to a real
	// weaveFS node that happens to be running on the same machine.
	serviceName := "weavefs-test-discovery"

	start := func() *Node {
		n, err := New(ctx, Config{
			ListenAddrs: []string{loopbackAddr},
			DataDir:     t.TempDir(),
			ServiceName: serviceName,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = n.Close() })
		return n
	}

	a, b := start(), start()

	if !a.DiscoveryActive() || !b.DiscoveryActive() {
		t.Skip("multicast is unavailable on this machine")
	}

	// Discovery is asynchronous, so poll rather than sleeping a fixed amount.
	require.Eventually(t, func() bool {
		for _, p := range a.Peers() {
			if p == b.Host().ID() {
				return true
			}
		}
		return false
	}, 25*time.Second, 250*time.Millisecond,
		"two nodes sharing a service name should discover and connect to each other")
}
