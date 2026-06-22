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

func (s *Store) writeStream(nodeID, key string, r io.Reader) (int64, error) {
	pathKey := s.PathTransformFunc(key)
	pathWithRoot := fmt.Sprintf("%s/%s/%s", s.Root, nodeID, pathKey.PathName)

	if err := os.MkdirAll(pathWithRoot, os.ModePerm); err != nil {
		return 0, err
	}

	filepath := fmt.Sprintf("%s/%s", pathWithRoot, pathKey.Filename)
	f, err := os.Create(filepath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, r)
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

func (s *Store) readStream(nodeID, key string) (int64, io.ReadCloser, error) {
	filepath := s.resolvedPath(nodeID, key)

	f, err := os.Open(filepath)
	if err != nil {
		return 0, nil, err
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, nil, err
	}

	return fi.Size(), f, nil
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

	// Prune oldest versions if MaxVersions is configured.
	if s.MaxVersions > 0 && len(idx.Versions) > s.MaxVersions {
		toRemove := idx.Versions[:len(idx.Versions)-s.MaxVersions]
		for _, old := range toRemove {
			_ = s.deleteVersionBlob(nodeID, key, old.VersionID)
		}
		idx.Versions = idx.Versions[len(idx.Versions)-s.MaxVersions:]
	}

	if err := saveIndex(idxFile, idx); err != nil {
		return VersionEntry{}, err
	}

	log.Printf("[store] wrote version %s (seq=%d) for key %q on node %s\n",
		versionID, seq, key, nodeID)
	return entry, nil
}

// ReadVersion returns a specific version blob; pass empty string for the latest.
func (s *Store) ReadVersion(nodeID, key, versionID string) (int64, io.ReadCloser, error) {
	mu := s.lockForKey(nodeID, key)
	mu.RLock()
	defer mu.RUnlock()

	idxFile := indexPath(s.Root, nodeID, key)
	idx, err := loadIndex(idxFile, key)
	if err != nil {
		return 0, nil, err
	}

	if versionID == "" {
		// Caller wants the latest version.
		if idx.Latest == "" {
			return 0, nil, fmt.Errorf("store: no versions found for key %q on node %s", key, nodeID)
		}
		versionID = idx.Latest
	} else {
		// Verify the requested version exists in the index.
		found := false
		for _, v := range idx.Versions {
			if v.VersionID == versionID {
				found = true
				break
			}
		}
		if !found {
			return 0, nil, fmt.Errorf("store: version %q not found for key %q on node %s",
				versionID, key, nodeID)
		}
	}

	return s.readStream(nodeID, versionedKey(key, versionID))
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
