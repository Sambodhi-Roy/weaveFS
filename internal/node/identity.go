package node

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/Sambodhi-Roy/weaveFS/internal/fsutil"
)

// identityFileName is the file, relative to the node's data directory, that
// holds the private key defining this node's identity on the network.
const identityFileName = "identity.key"

// identityFileMode restricts the key file to the owner. Windows does not
// enforce these bits, but on Unix it keeps the key out of other users' reach.
const identityFileMode = 0o600

// loadOrCreateIdentity returns the node's long-lived private key, generating
// and persisting a new one the first time a data directory is used.
//
// The key must be stable across restarts. A libp2p PeerID is derived from the
// public key, and internal/store scopes every path by that PeerID — so a new
// key means a new PeerID, which means the store silently starts writing to an
// empty directory and every previously stored file becomes unreachable.
//
// The key deliberately lives beside the per-node data directories rather than
// inside one: computing the directory name requires the PeerID, which requires
// the key. See docs segment-a-libp2p-node.md, decision 2.
func loadOrCreateIdentity(dataDir string) (crypto.PrivKey, error) {
	path := filepath.Join(dataDir, identityFileName)

	data, err := os.ReadFile(path)
	if err == nil {
		key, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("node: identity file %s is corrupt: %w", path, err)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("node: reading identity %s: %w", path, err)
	}

	return createIdentity(path)
}

// createIdentity generates a fresh Ed25519 key pair and writes the private key
// to path. Ed25519 is chosen over RSA for its small keys and fast generation;
// nothing in weaveFS needs RSA compatibility.
func createIdentity(path string) (crypto.PrivKey, error) {
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("node: generating identity: %w", err)
	}

	data, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("node: marshalling identity: %w", err)
	}

	if err := fsutil.WriteFileAtomic(path, data, identityFileMode); err != nil {
		return nil, fmt.Errorf("node: saving identity %s: %w", path, err)
	}
	return priv, nil
}
