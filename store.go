package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
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
}

// Store manages on-disk content-addressable storage for a single node.
type Store struct {
	StoreOpts
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

// Write streams data from r into the CAS store under the given nodeID and key.
// Returns the number of bytes written.
func (s *Store) Write(nodeID, key string, r io.Reader) (int64, error) {
	return s.writeStream(nodeID, key, r)
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

// Read opens the stored file and returns its size and a reader.
// The caller is responsible for closing the underlying file.
func (s *Store) Read(nodeID, key string) (int64, io.ReadCloser, error) {
	return s.readStream(nodeID, key)
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

// Has reports whether a file with the given key exists in the store
// for the specified nodeID.
func (s *Store) Has(nodeID, key string) bool {
	filepath := s.resolvedPath(nodeID, key)
	_, err := os.Stat(filepath)
	return !os.IsNotExist(err)
}

// Delete removes the entire top-level directory tree for the given key,
// effectively deleting all path segments and the file itself.
func (s *Store) Delete(nodeID, key string) error {
	rootPath := s.resolvedRootPath(nodeID, key)
	return os.RemoveAll(rootPath)
}

// Clear wipes the entire storage root directory.
// Use this in tests or when reinitialising a node from scratch.
func (s *Store) Clear() error {
	return os.RemoveAll(s.Root)
}
