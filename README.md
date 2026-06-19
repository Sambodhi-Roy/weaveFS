# weaveFS

A distributed file system written in Go, built incrementally.

## Development Progress

### Week 1: Project Skeleton, P2P Interfaces & TCP Transport
In the first week, we laid down the foundational boilerplate, core abstractions, and a fully working TCP transport layer:

- **Project Scaffold:** Initialized the Go module (`github.com/Sambodhi-Roy/weaveFS`) and set up a `Makefile` with `build`, `run`, and `test` targets.
- **P2P Interfaces:** Defined the core `Peer` and `Transport` interfaces in the `p2p/` package to decouple the networking logic from the rest of the system.
- **RPC Message Types:** Added the `RPC` struct with `IncomingMessage` and `IncomingStream` byte constants to distinguish regular messages from raw file streams.
- **Handshake:** Added a `HandshakeFunc` type and a `NOPHandshakeFunc` placeholder — real peer authentication can be plugged in here later.
- **Message Decoder:** Implemented a `DefaultDecoder` that reads a 1-byte type prefix to determine if incoming data is a message or a stream, without blocking the read loop.
- **TCP Transport:** Implemented `TCPPeer` (wraps `net.Conn`, uses a `sync.WaitGroup` to pause reads during file streams) and `TCPTransport` (manages listening, dialing, and routing incoming RPCs to a buffered channel).
- **Tests:** Added a passing test that verifies `TCPTransport` can successfully listen on a port.
- **Entry Point:** Set up `main.go` that compiles and runs cleanly.

## Getting Started

To build and run the current state of the project, make sure you have Go installed, then run:

```bash
make run
```

*(Note for Windows users: if `make` is not available on your terminal, you can run `go run main.go` directly, or use the provided `make.bat` wrapper by running `.\make run`).*

### Week 2: Content-Addressable Storage (CAS) Layer
In the second week, we built the on-disk storage engine that every node uses to persist and retrieve files locally:

- **`PathKey` & `PathTransformFunc`:** Defined a pluggable path derivation strategy. A `PathKey` holds the nested directory path and the content-hash filename derived from any string key.
- **`CASPathTransformFunc`:** SHA-1 hashes the key and splits the 40-char hex string into 8 chunks of 5 characters to form a deeply nested directory tree — preventing hotspot directories and enabling natural deduplication.
- **`DefaultPathTransformFunc`:** A passthrough transform (key → key) for simple testing scenarios.
- **`Store` struct:** The core storage engine, configured with a `Root` folder and a `PathTransformFunc`:
  - `Write(nodeID, key, reader)` — streams data into the CAS path, creating directories as needed.
  - `Read(nodeID, key)` — opens the stored file and returns its size + an `io.ReadCloser`.
  - `Has(nodeID, key)` — checks if a key exists on disk for a given node.
  - `Delete(nodeID, key)` — removes the entire top-level hash directory tree for a key.
  - `Clear()` — wipes the entire root (used in tests).
- **Node-scoped paths:** Files are stored under `<root>/<nodeID>/...` so multiple simulated nodes can coexist on the same machine without collisions.
- **Tests:** 4 passing tests covering the CAS path transform, write/read round-trip, `Has`, and `Delete`. Temp data is cleaned up via `defer s.Clear()`.
- **Smoke test:** `main.go` exercises a full write → read cycle, printing the stored bytes to confirm end-to-end plumbing.
