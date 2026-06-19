package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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
