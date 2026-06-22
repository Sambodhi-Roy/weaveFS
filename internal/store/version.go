package store

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// VersionEntry records metadata for a single stored version of a file.
type VersionEntry struct {
	// VersionID is the canonical UUID used to locate the blob on disk.
	// It is the stable, globally-unique identifier across all nodes.
	VersionID string `json:"version_id"`

	// Seq is a node-local, human-readable sequence number (1, 2, 3, …).
	// Display-only — must NOT be used as a CAS key or for cross-node comparison.
	Seq int `json:"seq"`

	// CreatedAt is the UTC wall-clock time when this version was written.
	CreatedAt time.Time `json:"created_at"`

	// SizeBytes is the number of bytes in the stored blob.
	SizeBytes int64 `json:"size_bytes"`

	// Message is an optional human-readable note about this version,
	// analogous to a Git commit message.
	Message string `json:"message,omitempty"`
}

// VersionIndex is the table of contents for all versions of a single logical key.
type VersionIndex struct {
	// Key is the logical file name given by the caller (e.g. "my_document").
	Key string `json:"key"`

	// Latest is the VersionID of the most recently written version.
	// An empty string means no versions have been written yet.
	Latest string `json:"latest"`

	// Versions is the ordered list of all version entries, oldest first.
	Versions []VersionEntry `json:"versions"`
}

// versionedKey produces the CAS input string for a specific version of a key.
// Appending the UUID ensures each version gets a unique, deterministic blob path
// without any coordination between nodes.
func versionedKey(key, versionID string) string {
	return key + "@" + versionID
}

// indexPath returns the filesystem path to the version index JSON file for a
// given (root, nodeID, key) triple. The key is SHA-1 hashed so any arbitrary
// string is safe to use as a filename.
//
// Layout: <root>/<nodeID>/.vindex/<sha1(key)>.json
func indexPath(root, nodeID, key string) string {
	h := sha1.Sum([]byte(key))
	return fmt.Sprintf("%s/%s/.vindex/%s.json", root, nodeID, hex.EncodeToString(h[:]))
}

// loadIndex reads and unmarshals the version index from disk.
// If the file does not yet exist it returns an empty, valid index for the key.
func loadIndex(path, key string) (*VersionIndex, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &VersionIndex{Key: key}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: loadIndex %s: %w", path, err)
	}

	var idx VersionIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("store: loadIndex unmarshal %s: %w", path, err)
	}
	return &idx, nil
}

// saveIndex atomically writes the version index to disk.
//
// It writes to a temporary file first, then calls os.Rename to swap it into
// place. On POSIX, rename(2) is atomic; on Windows it has worked reliably
// since Go 1.5. This guarantees we never leave a partially-written index file
// if the process crashes mid-write.
func saveIndex(path string, idx *VersionIndex) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("store: saveIndex marshal: %w", err)
	}

	// Ensure the .vindex parent directory exists.
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return fmt.Errorf("store: saveIndex mkdir: %w", err)
	}

	// Write to a sibling .tmp file so the rename stays on the same filesystem.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: saveIndex write tmp: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup on failure
		return fmt.Errorf("store: saveIndex rename: %w", err)
	}
	return nil
}

// newUUID generates a random UUID v4 using crypto/rand (no external dependency).
// Panics if the OS cannot provide entropy — that is a non-recoverable condition.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: failed to read random bytes for UUID: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // set version 4
	b[8] = (b[8] & 0x3f) | 0x80 // set RFC 4122 variant
	// %x on a []byte encodes each byte as exactly two lower-case hex digits.
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nextSeq returns one more than the highest Seq already in the index.
// Returns 1 when the index is empty.
func nextSeq(idx *VersionIndex) int {
	if len(idx.Versions) == 0 {
		return 1
	}
	return idx.Versions[len(idx.Versions)-1].Seq + 1
}
