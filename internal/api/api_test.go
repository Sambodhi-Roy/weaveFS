package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sambodhi-Roy/weaveFS/internal/crypto"
	"github.com/Sambodhi-Roy/weaveFS/internal/node"
	"github.com/Sambodhi-Roy/weaveFS/internal/server"
	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

var payload = []byte("weaveFS: distributed storage, one node at a time")

// testAPI is a complete node with its API running in front of it.
type testAPI struct {
	api    *API
	server *server.FileServer
	node   *node.Node
	dir    string
	base   string
}

// newTestAPI starts a node in a temporary directory and puts an API on it.
//
// Discovery is disabled: multicast is blocked in many CI runners, and a suite
// that waits on mDNS fails for reasons unrelated to the code under test.
func newTestAPI(t *testing.T) *testAPI {
	t.Helper()

	dir := t.TempDir()

	n, err := node.New(context.Background(), node.Config{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		DataDir:     dir,
		DisableMDNS: true,
	})
	require.NoError(t, err)
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

	srv, err := server.New(server.Config{Node: n, Store: st})
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Close() })

	a, err := New(Config{FileServer: srv, DataDir: dir})
	require.NoError(t, err)
	require.NoError(t, a.Start())
	t.Cleanup(func() { _ = a.Close() })

	return &testAPI{api: a, server: srv, node: n, dir: dir, base: "http://" + a.Addr()}
}

// put stores a body under a key and returns the decoded result.
func (ta *testAPI) put(t *testing.T, key string, body []byte) writeResultJSON {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut,
		ta.base+"/v1/files?key="+key, bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out writeResultJSON
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	_, err := New(Config{DataDir: "somewhere"})
	require.Error(t, err, "a FileServer is required")

	_, err = New(Config{FileServer: &server.FileServer{}})
	require.Error(t, err, "a DataDir is required")
}

// TestAddrFileIsWrittenAndRemoved covers how a client finds the node at all. A
// stale address file after shutdown would send the next put at a dead port.
func TestAddrFileIsWrittenAndRemoved(t *testing.T) {
	ta := newTestAPI(t)

	addr, err := ReadAddrFile(ta.dir)
	require.NoError(t, err)
	assert.Equal(t, ta.api.Addr(), addr, "the file must record the port actually bound")
	assert.True(t, strings.HasPrefix(addr, "127.0.0.1:"), "the API must bind loopback only")

	require.NoError(t, ta.api.Close())

	_, err = os.Stat(AddrFilePath(ta.dir))
	assert.True(t, os.IsNotExist(err), "the address file must not outlive the node")
}

func TestReadAddrFileOnMissingDirectory(t *testing.T) {
	_, err := ReadAddrFile(filepath.Join(t.TempDir(), "no_node_here"))
	require.Error(t, err)

	// errors.Is, not os.IsNotExist: the error is wrapped with %w, and only
	// errors.Is walks the chain. This is exactly the check the CLI performs to
	// turn a missing file into "no node is running here".
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"the error must be recognisable as 'not there' so the CLI can explain it")
}

// TestPutThenGet is the ordinary round trip through HTTP.
func TestPutThenGet(t *testing.T) {
	ta := newTestAPI(t)

	result := ta.put(t, "report", payload)
	assert.Equal(t, "report", result.Key)
	assert.Equal(t, int64(len(payload)), result.SizeBytes, "sizes are reported as plaintext")
	assert.Equal(t, 1, result.Seq)
	assert.NotEmpty(t, result.VersionID)

	resp, err := http.Get(ta.base + "/v1/files?key=report")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, int64(len(payload)), resp.ContentLength,
		"the announced length is the plaintext length, not the on-disk one")

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

// TestPutWithNoPeersReportsZero is the reason the API can be honest: a write
// that reached nobody must say so rather than looking identical to a success.
func TestPutWithNoPeersReportsZero(t *testing.T) {
	ta := newTestAPI(t)

	result := ta.put(t, "lonely", payload)
	require.Empty(t, ta.node.Peers(), "this test is only meaningful with no peers")

	assert.Zero(t, result.PeersTried)
	assert.Zero(t, result.PeersStored)
	assert.Empty(t, result.Failures)
}

func TestGetUnknownKeyIs404(t *testing.T) {
	ta := newTestAPI(t)

	resp, err := http.Get(ta.base + "/v1/files?key=never_written")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"a missing file is not a server malfunction")

	var payload map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.NotEmpty(t, payload["error"], "failures are JSON too, so clients parse one shape")
}

func TestMissingKeyParameterIs400(t *testing.T) {
	ta := newTestAPI(t)

	for _, path := range []string{"/v1/files", "/v1/versions"} {
		resp, err := http.Get(ta.base + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, path)
	}
}

// TestKeysWithSlashesSurvive is why the key is a query parameter rather than a
// path segment.
func TestKeysWithSlashesSurvive(t *testing.T) {
	ta := newTestAPI(t)

	const key = "notes/2026/q1 report"

	req, err := http.NewRequest(http.MethodPut,
		ta.base+"/v1/files?key="+urlEscape(key), bytes.NewReader(payload))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := http.Get(ta.base + "/v1/files?key=" + urlEscape(key))
	require.NoError(t, err)
	defer got.Body.Close()
	require.Equal(t, http.StatusOK, got.StatusCode)

	body, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, body)
}

func TestListVersions(t *testing.T) {
	ta := newTestAPI(t)

	ta.put(t, "doc", []byte("version one"))
	ta.put(t, "doc", []byte("version two"))

	resp, err := http.Get(ta.base + "/v1/versions?key=doc")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Key      string        `json:"key"`
		Versions []versionJSON `json:"versions"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	require.Len(t, out.Versions, 2, "both versions must be listed, oldest first")
	assert.Equal(t, 1, out.Versions[0].Seq)
	assert.Equal(t, 2, out.Versions[1].Seq)
	assert.Equal(t, int64(len("version one")), out.Versions[0].SizeBytes)
}

// TestGetSpecificVersion checks the older version is still reachable after a
// newer one lands, which is the whole point of keeping history.
func TestGetSpecificVersion(t *testing.T) {
	ta := newTestAPI(t)

	first := ta.put(t, "doc", []byte("version one"))
	ta.put(t, "doc", []byte("version two"))

	resp, err := http.Get(ta.base + "/v1/files?key=doc&version=" + first.VersionID)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, []byte("version one"), body, "asking for v1 must not return v2")
}

func TestDeleteRemovesEveryLocalVersion(t *testing.T) {
	ta := newTestAPI(t)

	ta.put(t, "doomed", payload)
	require.True(t, ta.server.Has("doomed"))

	req, err := http.NewRequest(http.MethodDelete, ta.base+"/v1/files?key=doomed", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.False(t, ta.server.Has("doomed"), "the local copy must be gone")

	// And with no peers to recover from, a get now fails rather than silently
	// returning something stale.
	got, err := http.Get(ta.base + "/v1/files?key=doomed")
	require.NoError(t, err)
	got.Body.Close()
	assert.Equal(t, http.StatusNotFound, got.StatusCode)
}

func TestWrongMethodIsRejected(t *testing.T) {
	ta := newTestAPI(t)

	req, err := http.NewRequest(http.MethodPost, ta.base+"/v1/files?key=x", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestIDReportsTheAnsweringNode(t *testing.T) {
	ta := newTestAPI(t)

	resp, err := http.Get(ta.base + "/v1/id")
	require.NoError(t, err)
	defer resp.Body.Close()

	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, ta.server.ID(), out["peer_id"])
}

// TestCloseIsIdempotent guards the shutdown path: serve defers Close, and a
// second call must not panic.
func TestCloseIsIdempotent(t *testing.T) {
	ta := newTestAPI(t)

	require.NoError(t, ta.api.Close())
	require.NoError(t, ta.api.Close())
}

// TestStartRejectsAPortInUse checks binding happens synchronously, so the error
// reaches the caller instead of a background goroutine nobody is watching.
func TestStartRejectsAPortInUse(t *testing.T) {
	first := newTestAPI(t)

	second, err := New(Config{
		FileServer: first.server,
		DataDir:    t.TempDir(),
		Addr:       first.api.Addr(),
	})
	require.NoError(t, err)

	err = second.Start()
	require.Error(t, err, "the second bind to the same port must fail")
	assert.Contains(t, err.Error(), "listening on")
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "%20", "/", "%2F").Replace(s)
}

// TestLargeBodyStreams checks a file bigger than a single buffer survives the
// round trip intact, since both directions stream rather than buffering.
func TestLargeBodyStreams(t *testing.T) {
	ta := newTestAPI(t)

	large := bytes.Repeat([]byte("weaveFS"), 200_000) // ~1.4 MB
	result := ta.put(t, "big", large)
	assert.Equal(t, int64(len(large)), result.SizeBytes)

	resp, err := http.Get(ta.base + "/v1/files?key=big")
	require.NoError(t, err)
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, len(large), len(got))
	assert.True(t, bytes.Equal(large, got), "every byte must survive the round trip")
}

// TestAddrFileIsPrivate mirrors the key files beside it. Skipped on Windows,
// where Unix permission bits are not enforced and a pass would prove nothing.
func TestAddrFileIsPrivate(t *testing.T) {
	if isWindows() {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	ta := newTestAPI(t)

	info, err := os.Stat(AddrFilePath(ta.dir))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func isWindows() bool { return os.PathSeparator == '\\' }

// TestShutdownTimeoutIsSane is a guard against someone setting the constant to
// zero, which would drop every in-flight transfer on shutdown.
func TestShutdownTimeoutIsSane(t *testing.T) {
	assert.Greater(t, shutdownTimeout, time.Second)
}

// TestShareEndpointValidatesTargeting checks the HTTP wiring of the share
// endpoint — routing, target parsing, and the errors it maps — without needing a
// second node. A successful share across two nodes is covered in internal/server.
func TestShareEndpointValidatesTargeting(t *testing.T) {
	ta := newTestAPI(t)
	require.Empty(t, ta.node.Peers(), "this test runs with no peers on purpose")

	share := func(t *testing.T, query string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ta.base+"/v1/share?"+query, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	t.Run("a missing key is a 400", func(t *testing.T) {
		resp := share(t, "all=true")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("no target is a 400", func(t *testing.T) {
		resp := share(t, "key=report")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("a target with no peers connected is a 400", func(t *testing.T) {
		resp := share(t, "key=report&all=true")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("a malformed peer id is a 400", func(t *testing.T) {
		resp := share(t, "key=report&peer=not-a-peer-id")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}
