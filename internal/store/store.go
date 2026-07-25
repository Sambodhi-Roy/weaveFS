package store

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultRootFolderName = "weavefs_data"

// PathKey holds the derived directory tree path and the final filename
// for a stored object.
type PathKey struct {
	// PathName is the nested directory path, e.g. "68044/29f74/181a6/..."
	PathName string
	// Filename is the full hex-encoded hash used as the actual file name.
	Filename string
}

// FirstPathName returns the top-level directory component of the path.
// Used when deleting an entire object subtree.
func (p PathKey) FirstPathName() string {
	parts := strings.Split(p.PathName, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// FullPath returns the complete relative path including the filename.
func (p PathKey) FullPath() string {
	return fmt.Sprintf("%s/%s", p.PathName, p.Filename)
}

// PathTransformFunc is a pluggable strategy for deriving a storage path
// from an arbitrary string key.
type PathTransformFunc func(key string) PathKey

// Cipher transforms blob content as it streams to and from disk.
//
// The interface is declared here, in the package that consumes it, rather than
// alongside its implementation in internal/crypto. That is the Go convention,
// and it means the dependency points one way: store never imports crypto, and
// tests can substitute a fake without pulling real cryptography in.
//
// A nil Cipher means blobs are stored as plaintext, which is the zero value and
// therefore the default.
type Cipher interface {
	// EncryptWriter returns a writer that encrypts everything written to it
	// into w, having first written any header of its own to w.
	EncryptWriter(w io.Writer) (io.Writer, error)

	// DecryptReader returns a reader yielding plaintext from the ciphertext in
	// r, having first consumed and validated any header.
	DecryptReader(r io.Reader) (io.Reader, error)

	// Overhead reports how many bytes larger a stored blob is than its
	// plaintext.
	Overhead() int64
}

// CASPathTransformFunc implements content-addressable storage path derivation.
// It SHA-1 hashes the key, then splits the 40-char hex string into 8 chunks
// of 5 characters each to form a nested directory tree.
//
// Example:
//
//	key  → "momsbestpicture"
//	hash → "6804429f74181a63c50c3d81d733a12f14a353ff"
//	path → "68044/29f74/181a6/3c50c/3d81d/733a1/2f14a/353ff"
func CASPathTransformFunc(key string) PathKey {
	hash := sha1.Sum([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	const blockSize = 5
	numBlocks := len(hashStr) / blockSize
	parts := make([]string, numBlocks)

	for i := 0; i < numBlocks; i++ {
		from := i * blockSize
		parts[i] = hashStr[from : from+blockSize]
	}

	return PathKey{
		PathName: strings.Join(parts, "/"),
		Filename: hashStr,
	}
}

// DefaultPathTransformFunc is a passthrough — it stores files by their raw key.
// Useful for tests that don't need hashed paths.
var DefaultPathTransformFunc PathTransformFunc = func(key string) PathKey {
	return PathKey{
		PathName: key,
		Filename: key,
	}
}

// StoreOpts configures a Store instance.
type StoreOpts struct {
	// Root is the top-level folder that contains all node storage directories.
	// Defaults to defaultRootFolderName if empty.
	Root string
	// PathTransformFunc determines how a key maps to a filesystem path.
	// Defaults to DefaultPathTransformFunc.
	PathTransformFunc PathTransformFunc
	// MaxVersions is the maximum number of versions to retain per key.
	// When a new write would exceed this limit the oldest versions are
	// automatically pruned. 0 (the default) means keep all versions.
	MaxVersions int
	// Cipher encrypts blob content at rest. nil (the default) stores
	// plaintext. Note that a Store cannot read blobs written under a
	// different setting: switching this on or off orphans existing data.
	Cipher Cipher
}

// Store manages on-disk content-addressable storage for a single node.
type Store struct {
	StoreOpts
	// mu holds one *sync.RWMutex per (nodeID+":"+key) pair.
	// Per-key locking means concurrent writes to different keys never block
	// each other, while concurrent writes to the same key are serialised.
	mu sync.Map
}

// NewStore creates a new Store with the given options.
func NewStore(opts StoreOpts) *Store {
	if opts.PathTransformFunc == nil {
		opts.PathTransformFunc = DefaultPathTransformFunc
	}
	if opts.Root == "" {
		opts.Root = defaultRootFolderName
	}
	return &Store{StoreOpts: opts}
}

// resolvedPath returns the full OS path for a given nodeID and key.
func (s *Store) resolvedPath(nodeID, key string) string {
	pathKey := s.PathTransformFunc(key)
	return fmt.Sprintf("%s/%s/%s", s.Root, nodeID, pathKey.FullPath())
}

// resolvedRootPath returns the top-level hash directory (used for deletion).
func (s *Store) resolvedRootPath(nodeID, key string) string {
	pathKey := s.PathTransformFunc(key)
	return fmt.Sprintf("%s/%s/%s", s.Root, nodeID, pathKey.FirstPathName())
}

// lockForKey returns the *sync.RWMutex associated with the given (nodeID, key)
// pair, creating one on first use. Multiple callers for the same pair share
// the same mutex; callers for different pairs get independent mutexes.
func (s *Store) lockForKey(nodeID, key string) *sync.RWMutex {
	mapKey := nodeID + ":" + key
	actual, _ := s.mu.LoadOrStore(mapKey, &sync.RWMutex{})
	return actual.(*sync.RWMutex)
}

// Write saves data under key, always creating a new version. Delegates to WriteVersion.
func (s *Store) Write(nodeID, key string, r io.Reader) (int64, error) {
	entry, err := s.WriteVersion(nodeID, key, "", r)
	if err != nil {
		return 0, err
	}
	return entry.SizeBytes, nil
}

// createBlobFile creates the directory tree for a blob and opens the file for
// writing, returning the handle and the path it was created at.
//
// It is shared by the encrypting and the verbatim write paths, which differ
// only in what they wrap around the returned file.
func (s *Store) createBlobFile(nodeID, key string) (*os.File, string, error) {
	pathKey := s.PathTransformFunc(key)
	pathWithRoot := fmt.Sprintf("%s/%s/%s", s.Root, nodeID, pathKey.PathName)

	if err := os.MkdirAll(pathWithRoot, os.ModePerm); err != nil {
		return nil, "", err
	}

	filepath := fmt.Sprintf("%s/%s", pathWithRoot, pathKey.Filename)
	f, err := os.Create(filepath)
	if err != nil {
		return nil, "", err
	}
	return f, filepath, nil
}

// writeStream copies r to the blob's CAS path, encrypting on the way if a
// Cipher is configured. The returned count is always plaintext bytes: it comes
// from io.Copy, which reports what it read from r, and the cipher's header is
// written before the copy begins so it never passes through that count.
func (s *Store) writeStream(nodeID, key string, r io.Reader) (int64, error) {
	f, filepath, err := s.createBlobFile(nodeID, key)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// dst is the file itself when storing plaintext, or a decorator wrapping it
	// when encrypting. io.Copy cannot tell the difference — both are io.Writer.
	var dst io.Writer = f
	if s.Cipher != nil {
		dst, err = s.Cipher.EncryptWriter(f)
		if err != nil {
			return 0, fmt.Errorf("store: writeStream: %w", err)
		}
	}

	n, err := io.Copy(dst, r)
	if err != nil {
		return 0, err
	}

	log.Printf("[store] wrote %d bytes to %s\n", n, filepath)
	return n, nil
}

// Read returns the latest version's size and a ReadCloser. Delegates to ReadVersion.
func (s *Store) Read(nodeID, key string) (int64, io.ReadCloser, error) {
	return s.ReadVersion(nodeID, key, "")
}

// decryptedFile pairs a decrypting reader with the file underneath it, so that
// closing the value the caller was handed still closes the real file descriptor.
// The embedded io.Reader supplies Read; only Close needs writing.
type decryptedFile struct {
	io.Reader
	f *os.File
}

// Close releases the underlying file. The decrypting reader itself holds no
// resources — CTR keeps only a counter — so there is nothing else to release.
func (d *decryptedFile) Close() error { return d.f.Close() }

// readStream opens a blob, decrypting it if a Cipher is configured.
//
// It deliberately does not report a size. The file on disk is larger than its
// plaintext once encrypted, and the authoritative plaintext length already
// lives in the version index, so ReadVersion supplies it from there instead of
// this function re-deriving it with a stat and some arithmetic.
func (s *Store) readStream(nodeID, key string) (io.ReadCloser, error) {
	filepath := s.resolvedPath(nodeID, key)

	f, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}

	if s.Cipher == nil {
		return f, nil
	}

	plaintext, err := s.Cipher.DecryptReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("store: readStream: %s: %w", filepath, err)
	}
	return &decryptedFile{Reader: plaintext, f: f}, nil
}

// Has reports whether key has at least one stored version on nodeID.
func (s *Store) Has(nodeID, key string) bool {
	mu := s.lockForKey(nodeID, key)
	mu.RLock()
	defer mu.RUnlock()

	idx, err := loadIndex(indexPath(s.Root, nodeID, key), key)
	if err != nil {
		return false
	}
	return idx.Latest != ""
}

// Delete removes ALL versions of key for nodeID, including the version index.
// NOTE: this is permanent and removes complete history.
func (s *Store) Delete(nodeID, key string) error {
	mu := s.lockForKey(nodeID, key)
	mu.Lock()
	defer mu.Unlock()

	idxFile := indexPath(s.Root, nodeID, key)
	idx, err := loadIndex(idxFile, key)
	if err != nil {
		return err
	}
	for _, v := range idx.Versions {
		_ = s.deleteVersionBlob(nodeID, key, v.VersionID)
	}
	_ = os.Remove(idxFile)
	return nil
}

// Clear wipes the entire storage root directory.
// Use this in tests or when reinitialising a node from scratch.
func (s *Store) Clear() error {
	return os.RemoveAll(s.Root)
}

// WriteVersion saves a new immutable version blob for key and updates the version index.
func (s *Store) WriteVersion(nodeID, key, message string, r io.Reader) (VersionEntry, error) {
	mu := s.lockForKey(nodeID, key)
	mu.Lock()
	defer mu.Unlock()

	idxFile := indexPath(s.Root, nodeID, key)
	idx, err := loadIndex(idxFile, key)
	if err != nil {
		return VersionEntry{}, err
	}

	versionID := newUUID()
	seq := nextSeq(idx)

	// Write the blob using the existing low-level writer.
	// The CAS path is derived from key@versionID so every version is unique.
	n, err := s.writeStream(nodeID, versionedKey(key, versionID), r)
	if err != nil {
		return VersionEntry{}, err
	}

	entry := VersionEntry{
		VersionID: versionID,
		Seq:       seq,
		CreatedAt: time.Now().UTC(),
		SizeBytes: n,
		Message:   message,
	}

	idx.Versions = append(idx.Versions, entry)
	idx.Latest = versionID

	s.pruneOldestVersions(nodeID, key, idx)

	if err := saveIndex(idxFile, idx); err != nil {
		return VersionEntry{}, err
	}

	log.Printf("[store] wrote version %s (seq=%d) for key %q on node %s\n",
		versionID, seq, key, nodeID)
	return entry, nil
}

// pruneOldestVersions trims the index to MaxVersions entries, deleting the
// blobs it drops. It does nothing when MaxVersions is 0, the default, which
// means keep everything. Callers must hold the write lock.
func (s *Store) pruneOldestVersions(nodeID, key string, idx *VersionIndex) {
	if s.MaxVersions <= 0 || len(idx.Versions) <= s.MaxVersions {
		return
	}

	cut := len(idx.Versions) - s.MaxVersions
	for _, old := range idx.Versions[:cut] {
		_ = s.deleteVersionBlob(nodeID, key, old.VersionID)
	}
	idx.Versions = idx.Versions[cut:]
}

// resolveVersion finds the entry for versionID, treating an empty versionID as
// a request for the latest. It both verifies the version exists and supplies
// its recorded metadata.
func resolveVersion(idx *VersionIndex, nodeID, key, versionID string) (VersionEntry, error) {
	if versionID == "" {
		if idx.Latest == "" {
			return VersionEntry{}, fmt.Errorf(
				"store: no versions found for key %q on node %s", key, nodeID)
		}
		versionID = idx.Latest
	}

	for _, v := range idx.Versions {
		if v.VersionID == versionID {
			return v, nil
		}
	}
	return VersionEntry{}, fmt.Errorf("store: version %q not found for key %q on node %s",
		versionID, key, nodeID)
}

// ReadVersion returns a specific version blob; pass empty string for the latest.
//
// The size returned is the plaintext length, taken from the version index
// rather than from the file on disk — those differ once encryption is enabled,
// and the index is the authoritative record of what was written.
func (s *Store) ReadVersion(nodeID, key, versionID string) (int64, io.ReadCloser, error) {
	mu := s.lockForKey(nodeID, key)
	mu.RLock()
	defer mu.RUnlock()

	idx, err := loadIndex(indexPath(s.Root, nodeID, key), key)
	if err != nil {
		return 0, nil, err
	}

	entry, err := resolveVersion(idx, nodeID, key, versionID)
	if err != nil {
		return 0, nil, err
	}

	rc, err := s.readStream(nodeID, versionedKey(key, entry.VersionID))
	if err != nil {
		return 0, nil, err
	}
	return entry.SizeBytes, rc, nil
}

// ListVersions returns a copy of the ordered version history for key, oldest first.
func (s *Store) ListVersions(nodeID, key string) ([]VersionEntry, error) {
	mu := s.lockForKey(nodeID, key)
	mu.RLock()
	defer mu.RUnlock()

	idxFile := indexPath(s.Root, nodeID, key)
	idx, err := loadIndex(idxFile, key)
	if err != nil {
		return nil, err
	}

	// Return a defensive copy so callers cannot mutate the live index slice.
	out := make([]VersionEntry, len(idx.Versions))
	copy(out, idx.Versions)
	return out, nil
}

// deleteVersionBlob removes a single version blob from disk; callers must hold the write lock.
func (s *Store) deleteVersionBlob(nodeID, key, versionID string) error {
	rootPath := s.resolvedRootPath(nodeID, versionedKey(key, versionID))
	return os.RemoveAll(rootPath)
}

// DeleteVersion removes one version blob and prunes it from the index. Rewinds Latest if needed.
func (s *Store) DeleteVersion(nodeID, key, versionID string) error {
	mu := s.lockForKey(nodeID, key)
	mu.Lock()
	defer mu.Unlock()

	idxFile := indexPath(s.Root, nodeID, key)
	idx, err := loadIndex(idxFile, key)
	if err != nil {
		return err
	}

	// Find the version and rebuild the slice without it.
	found := false
	filtered := idx.Versions[:0]
	for _, v := range idx.Versions {
		if v.VersionID == versionID {
			found = true
			continue // skip — this is the one being deleted
		}
		filtered = append(filtered, v)
	}
	if !found {
		return fmt.Errorf("store: version %q not found for key %q on node %s",
			versionID, key, nodeID)
	}
	idx.Versions = filtered

	// Rewind Latest if we just removed it.
	if idx.Latest == versionID {
		if len(idx.Versions) > 0 {
			idx.Latest = idx.Versions[len(idx.Versions)-1].VersionID
		} else {
			idx.Latest = ""
		}
	}

	// Remove the blob from disk before saving the updated index.
	if err := s.deleteVersionBlob(nodeID, key, versionID); err != nil {
		return err
	}
	return saveIndex(idxFile, idx)
}

// RollbackTo re-writes an old version as a new entry, making it the latest while preserving history.
func (s *Store) RollbackTo(nodeID, key, versionID string) (VersionEntry, error) {
	// Read the target version's content (acquires and releases the read lock).
	_, rc, err := s.ReadVersion(nodeID, key, versionID)
	if err != nil {
		return VersionEntry{}, fmt.Errorf("store: RollbackTo: cannot read version %q: %w", versionID, err)
	}
	defer rc.Close()

	// Write it as a new version (acquires the write lock independently).
	msg := fmt.Sprintf("rollback to version %s", versionID)
	return s.WriteVersion(nodeID, key, msg, rc)
}
