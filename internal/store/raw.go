package store

import (
	"fmt"
	"io"
	"log"
	"os"
)

// This file holds the two operations that move blob bytes without understanding
// them, so that one node can hand a blob to another for safekeeping.
//
// Every other read and write in this package treats a blob as content: it
// encrypts on the way to disk and decrypts on the way back, so a caller sees
// plaintext and never learns that encryption happened. That is exactly wrong
// for replication. When node A asks node B to keep a copy, the bytes that cross
// the network are A's ciphertext, and B — which has no key that could decrypt
// them — must write them down untouched. B is a custodian, not a reader.
//
// The pairing is what makes it work: ReadVersionRaw on the origin produces
// exactly what WriteVersionRaw on the custodian consumes, and when the origin
// later fetches its blob back, the bytes returned are the very bytes it
// encrypted. It writes them raw and then reads them through the ordinary
// decrypting path.

// ReadVersionRaw returns a version's blob exactly as it sits on disk — with the
// cipher's header and IV still attached, and the content still encrypted —
// together with the number of bytes that will be read.
//
// Pass an empty versionID for the latest version.
//
// The size comes from os.Stat rather than from the version index, which is the
// opposite of what every other read in this package does. That is deliberate.
// ReadVersion documents why it uses the index: SizeBytes means *plaintext*
// length, and an encrypted file on disk is larger. Here the caller is going to
// copy the file itself, byte for byte, so the physical length is the one that
// is true. Do not "fix" this to match its neighbours.
func (s *Store) ReadVersionRaw(nodeID, key, versionID string) (int64, io.ReadCloser, error) {
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

	path := s.resolvedPath(nodeID, versionedKey(key, entry.VersionID))

	info, err := os.Stat(path)
	if err != nil {
		return 0, nil, fmt.Errorf("store: ReadVersionRaw: %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("store: ReadVersionRaw: %s: %w", path, err)
	}
	return info.Size(), f, nil
}

// WriteVersionRaw stores bytes verbatim, bypassing the Cipher entirely, and
// records meta in the version index exactly as given.
//
// Preserving meta rather than minting fresh values is the whole point. A
// version's identity must be the same on every node that holds it, or the
// origin could never ask a custodian for one specific version. VersionID, Seq,
// CreatedAt, SizeBytes and Message therefore all come from the originating
// node, and SizeBytes stays the *plaintext* length even though this function
// never sees plaintext.
//
// Writing a version that is already present replaces it rather than appending a
// duplicate, so a replication attempt that is retried after a partial failure
// converges instead of corrupting the index.
func (s *Store) WriteVersionRaw(nodeID, key string, meta VersionEntry, r io.Reader) error {
	if meta.VersionID == "" {
		return fmt.Errorf("store: WriteVersionRaw: meta.VersionID is required, " +
			"since the originating node's version identity must be preserved")
	}

	mu := s.lockForKey(nodeID, key)
	mu.Lock()
	defer mu.Unlock()

	idxFile := indexPath(s.Root, nodeID, key)
	idx, err := loadIndex(idxFile, key)
	if err != nil {
		return err
	}

	// The blob is written before the index is touched. A failure here leaves an
	// orphaned file on disk but no index entry pointing at it, and the index is
	// the authoritative record of what exists — so the store stays consistent
	// and the stray bytes are merely wasted space.
	n, err := s.writeStreamRaw(nodeID, versionedKey(key, meta.VersionID), r)
	if err != nil {
		return err
	}

	idx.Versions = upsertVersion(idx.Versions, meta)
	idx.Latest = meta.VersionID

	s.pruneOldestVersions(nodeID, key, idx)

	if err := saveIndex(idxFile, idx); err != nil {
		return err
	}

	log.Printf("[store] stored %d raw bytes for version %s of key %q under node %s\n",
		n, meta.VersionID, key, nodeID)
	return nil
}

// upsertVersion replaces the entry sharing meta's VersionID, or appends meta if
// there is none. The returned slice keeps its oldest-first ordering.
func upsertVersion(versions []VersionEntry, meta VersionEntry) []VersionEntry {
	for i, v := range versions {
		if v.VersionID == meta.VersionID {
			versions[i] = meta
			return versions
		}
	}
	return append(versions, meta)
}

// writeStreamRaw copies r to the blob's CAS path with no transformation at all,
// so the bytes on disk are the bytes that arrived.
//
// It is the sibling of writeStream, which encrypts when a Cipher is set. The
// two are kept as separate functions rather than one function with a flag,
// because a call reading writeStream(..., false) tells you nothing about what
// false means.
func (s *Store) writeStreamRaw(nodeID, key string, r io.Reader) (int64, error) {
	f, filepath, err := s.createBlobFile(nodeID, key)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, r)
	if err != nil {
		return 0, fmt.Errorf("store: writeStreamRaw: %s: %w", filepath, err)
	}
	return n, nil
}
