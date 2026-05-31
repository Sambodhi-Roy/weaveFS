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
