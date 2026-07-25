// Command weavefs is currently a smoke test rather than a real entry point.
//
// It writes a file, reads it back, and then shows the raw bytes sitting on disk
// to demonstrate that they are ciphertext. A proper CLI arrives in Segment D,
// once there is a FileServer to drive.
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/Sambodhi-Roy/weaveFS/internal/crypto"
	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

const (
	dataDir = "weavefs_data"
	nodeID  = "local-node"
	key     = "my_document"
)

func main() {
	// The key is generated on first run and reused on every run after that.
	// Losing it means losing every blob under dataDir.
	encryptionKey, err := crypto.LoadOrCreateKey(dataDir)
	if err != nil {
		log.Fatalf("key error: %v", err)
	}

	cipher, err := crypto.NewAESCipher(encryptionKey)
	if err != nil {
		log.Fatalf("cipher error: %v", err)
	}

	s := store.NewStore(store.StoreOpts{
		Root:              dataDir,
		PathTransformFunc: store.CASPathTransformFunc,
		Cipher:            cipher,
	})

	data := []byte("weaveFS: distributed storage, one node at a time")

	// Write to the CAS store, encrypting on the way to disk.
	n, err := s.Write(nodeID, key, bytes.NewReader(data))
	if err != nil {
		log.Fatalf("write error: %v", err)
	}
	fmt.Printf("wrote %d bytes for key %q\n", n, key)

	// Read it back, decrypting on the way out.
	size, rc, err := s.Read(nodeID, key)
	if err != nil {
		log.Fatalf("read error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		log.Fatalf("read error: %v", err)
	}
	fmt.Printf("read  %d bytes: %q\n", size, string(got))

	showBlobOnDisk(data)
}

// showBlobOnDisk prints the first bytes of the most recently stored blob, so
// that the difference between what the API returns and what is actually
// persisted is visible rather than merely asserted in a test.
func showBlobOnDisk(plaintext []byte) {
	path, err := newestBlobPath(filepath.Join(dataDir, nodeID))
	if err != nil {
		log.Printf("could not locate blob on disk: %v", err)
		return
	}

	raw, err := readAtMost(path, 48)
	if err != nil {
		log.Printf("could not read blob: %v", err)
		return
	}

	fmt.Printf("\non disk at %s\n", path)
	fmt.Printf("  hex     : %s\n", hex.EncodeToString(raw))
	fmt.Printf("  as text : %q\n", sanitise(raw))
	fmt.Printf("  contains the plaintext: %v\n", bytes.Contains(raw, plaintext))
}

// newestBlobPath returns the most recently modified blob under nodeDir,
// skipping the .vindex directory, which holds version metadata rather than
// content.
func newestBlobPath(nodeDir string) (string, error) {
	var newest string
	var newestMod int64

	err := filepath.WalkDir(nodeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".vindex" {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if mod := info.ModTime().UnixNano(); mod >= newestMod {
			newest, newestMod = path, mod
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if newest == "" {
		return "", fmt.Errorf("no blob found under %s", nodeDir)
	}
	return newest, nil
}

// readAtMost reads up to limit bytes from path.
func readAtMost(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return io.ReadAll(io.LimitReader(f, limit))
}

// sanitise replaces bytes that are not printable ASCII with a dot, the way a
// hex editor does, so the output stays readable in a terminal.
func sanitise(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 0x20 && c < 0x7f {
			out[i] = c
			continue
		}
		out[i] = '.'
	}
	return string(out)
}
