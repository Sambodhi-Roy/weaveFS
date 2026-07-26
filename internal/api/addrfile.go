package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sambodhi-Roy/weaveFS/internal/fsutil"
)

// AddrFileName is the file inside a node's data directory that records where
// its API is listening. A client reads it to find the port the operating system
// handed out.
const AddrFileName = "api.addr"

// addrFileMode is 0600 — owner read and write only — matching identity.key and
// encryption.key beside it. There is no secret in an address, but anything that
// grants a handle on this node's storage belongs to the user who owns the node.
const addrFileMode = 0600

// AddrFilePath returns where the address file lives for a given data directory.
func AddrFilePath(dataDir string) string {
	return filepath.Join(dataDir, AddrFileName)
}

// ReadAddrFile returns the API address recorded for a node directory.
//
// A missing file is the single most likely thing to go wrong for a user — it
// means no node is running — so the caller is expected to detect that case and
// say so, rather than passing a bare path and errno along.
//
// Test for it with errors.Is(err, os.ErrNotExist), not os.IsNotExist. The error
// returned here is wrapped with %w, and errors.Is walks that chain while the
// older os.IsNotExist does not: it inspects only the error it is handed and
// would quietly report false.
func ReadAddrFile(dataDir string) (string, error) {
	path := AddrFilePath(dataDir)

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("api: reading %s: %w", path, err)
	}

	addr := strings.TrimSpace(string(data))
	if addr == "" {
		return "", fmt.Errorf("api: %s is empty", path)
	}
	return addr, nil
}

// writeAddrFile records the address the API bound to.
//
// It goes through fsutil.WriteFileAtomic — a temporary sibling file renamed
// over the target — so a client can never read a half-written address. The same
// helper writes both key files, which is the point of having it.
func writeAddrFile(dataDir, addr string) error {
	return fsutil.WriteFileAtomic(AddrFilePath(dataDir), []byte(addr+"\n"), addrFileMode)
}

// removeAddrFile deletes the address file on shutdown.
//
// Failures are ignored on purpose: this runs during shutdown, there is nothing
// useful to do about it, and a stale file only costs the next client one clear
// "connection refused" rather than any real harm.
func removeAddrFile(dataDir string) {
	_ = os.Remove(AddrFilePath(dataDir))
}
