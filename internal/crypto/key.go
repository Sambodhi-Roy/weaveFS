package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sambodhi-Roy/weaveFS/internal/fsutil"
)

// KeySize is the required key length in bytes. 32 bytes is AES-256.
const KeySize = 32

// keyFileName is the file, relative to the node's data directory, holding the
// key that every blob on this node is encrypted with.
const keyFileName = "encryption.key"

// keyFileMode restricts the key file to its owner, the same protection an SSH
// private key gets. Windows does not enforce these bits.
const keyFileMode = 0o600

// LoadOrCreateKey returns this node's blob encryption key, generating and
// persisting a new one the first time a data directory is used.
//
// The key must be stable for the life of the data directory. Every blob under
// it is encrypted with this key and there is no re-key path, so replacing the
// file makes every stored file permanently unreadable — the same class of
// silent, total loss that regenerating identity.key causes for stored paths.
//
// The key lives beside the per-node directories rather than inside one, next to
// identity.key. For identity.key that placement is forced, since computing the
// directory name requires the key; here it is a choice, so that a node's two
// secrets are one directory to configure, back up and protect.
//
// Note that store.Clear removes the whole root directory and so destroys this
// file along with the blobs it protects. Clear is documented as being for tests
// and for resetting a node from scratch, where that is the intent.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, keyFileName)

	data, err := os.ReadFile(path)
	if err == nil {
		key, err := decodeKey(data)
		if err != nil {
			return nil, fmt.Errorf("crypto: key file %s is corrupt: %w", path, err)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("crypto: reading key %s: %w", path, err)
	}

	return createKey(path)
}

// createKey generates a fresh random key and writes it to path.
//
// The randomness comes from crypto/rand, which draws on the operating system's
// entropy pool. This is not the math/rand used for simulations: that one is
// deterministically seeded and entirely predictable to an attacker.
func createKey(path string) ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("crypto: generating key: %w", err)
	}

	// Stored hex-encoded rather than raw so the file can be inspected, copied
	// and length-checked by hand, and so a truncated or mangled file fails to
	// decode loudly instead of silently becoming a different valid key.
	encoded := []byte(hex.EncodeToString(key))

	if err := fsutil.WriteFileAtomic(path, encoded, keyFileMode); err != nil {
		return nil, fmt.Errorf("crypto: saving key %s: %w", path, err)
	}
	return key, nil
}

// decodeKey parses the hex contents of a key file, tolerating surrounding
// whitespace so that a trailing newline added by an editor is not fatal.
func decodeKey(data []byte) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("expected %d hex characters: %w", KeySize*2, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("expected a %d-byte key, got %d bytes", KeySize, len(key))
	}
	return key, nil
}
