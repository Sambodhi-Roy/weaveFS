<h1 align="center">weaveFS</h1>
<p align="center"><b>A distributed file system built incrementally in Go — encryption, versioning, and peer replication from first principles.</b></p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25.7+-00ADD8?logo=go&logoColor=white" alt="Go 1.25.7+">
  <img src="https://img.shields.io/badge/transport-libp2p-blueviolet" alt="libp2p">
  <img src="https://img.shields.io/badge/encryption-AES--256--CTR-green" alt="AES-256-CTR">
</p>

Your files live on one machine. That machine dies. Your files are gone. weaveFS fixes that — not by trusting a cloud provider with the plaintext, but by keeping peers as **custodians**: they hold encrypted bytes that only your node can decrypt. You get durability without surrender.

> **A custodian holds your ciphertext, not your file.** When peer B stores your quarterly report, it writes the encrypted blob and can never read it — the key never leaves your node. If your disk dies, B hands the ciphertext back and you decrypt it. If B disappears, the data is still yours. The two failure modes are kept honest and separate.

weaveFS does what a single-machine program cannot:

- **Encrypts at rest and in transit.** AES-256-CTR on disk; Noise-encrypted connections in flight. Both halves closed.
- **Versions everything, non-destructively.** Every write creates a new immutable version. Roll back, list history, prune old copies — nothing is overwritten silently.
- **Replicates without trusting peers.** Peers hold your ciphertext. Fetching is almost trivial: the same bytes that went out come back, then your node decrypts them.
- **Shares files, not just backups.** `send` gives a peer a *readable* copy it owns outright — decrypt on the way out, re-encrypt under the recipient's key on the way in.
- **Self-discovers on a LAN.** mDNS finds peers without you specifying addresses, the same way AirDrop does.

---

## Install

Requires Go 1.25.7 or later.

```bash
git clone https://github.com/Sambodhi-Roy/weaveFS
cd weaveFS
make install
```

`make install` builds the `weavefs` binary into Go's bin directory (`%USERPROFILE%\go\bin` on Windows, `$GOPATH/bin` otherwise), so `weavefs ...` works from any directory. If the command is not found afterwards, add that directory to your `PATH`.

To avoid repeating `-data DIR` on every command, set it once per shell:

```powershell
$env:WEAVEFS_DATA = "node_a"   # PowerShell
# bash: export WEAVEFS_DATA=node_a
```

An explicit `-data` always overrides the environment variable.

---

## The everyday loop

Three terminals. On a normal LAN the nodes find each other without `-peer`, via mDNS — it is given explicitly below because multicast is blocked in many containers.

```bash
# terminal 1 — start node A
weavefs serve -data node_a
#   [node] listening on /ip4/127.0.0.1/tcp/57693/p2p/12D3KooWQfAd...
#   [api]  client API on 127.0.0.1:51023

# terminal 2 — start node B, peered to A
weavefs serve -data node_b -peer /ip4/127.0.0.1/tcp/57693/p2p/12D3KooWQfAd...

# terminal 3 — do work
weavefs put -data node_a -m "the quarterly numbers" report ./report.pdf
#   stored report v1 (19.5 KB) locally
#   replicated to 1 of 1 peer

weavefs ls -data node_a report
#   report — 1 version, oldest first
#     v1   19.5 KB   2026-07-26 03:43:29   3bfb1528-423d-4afc-96ca-355e6f2512a0
#          the quarterly numbers

weavefs rm  -data node_a report        # A throws its local copy away
weavefs get -data node_a report ./recovered.pdf
#   wrote 19.5 KB to ./recovered.pdf  <- fetched from B, decrypted with A's key
```

`recovered.pdf` is byte-for-byte identical to the original. Node A no longer had the file; node B handed back the ciphertext it had been keeping, and A decrypted it.

**To share a readable copy** — not a backup, but a real handover:

```bash
weavefs send -data node_a -peer 12D3KooWKvMN... report
# B now owns a readable copy, re-encrypted under B's own key.
# It is not a backup. It cannot be recalled.

weavefs send -data node_a -all report ./notes.txt
# fan out a loose file to every connected peer in one command
```

---

## What's inside

```
internal/
  store/    content-addressable storage, versioning, per-key locking, optional encryption
  crypto/   AES-256-CTR streaming codec: EncryptWriter, DecryptReader, per-node key
  fsutil/   atomic file replacement used by both key-file writers
  node/     libp2p host: persistent Ed25519 identity, Noise transport, mDNS discovery
  proto/    the framed request/response messages nodes exchange
  server/   FileServer: joins node and store - replication, recovery, sharing, outcome reporting
  api/      loopback HTTP API so client commands talk to a live node, not a throwaway process
cmd/
  weavefs/  demo, serve, id, put, send, get, ls, rm
```

### Storage — `internal/store`

Every object lives at a content-derived path: the key is SHA-1-hashed and the 40-char hex split into eight 5-character directory segments. No single directory ever grows large; identical content always lands at an identical path.

- **`Write` / `Read` / `Has` / `Delete` / `Clear`** — the basic surface.
- **`WriteVersion` / `ReadVersion` / `ListVersions` / `DeleteVersion` / `RollbackTo`** — the versioned surface. `Write` and `Read` are thin wrappers, so nothing breaks by going deeper.
- **Version IDs are UUIDv4.** A human-readable sequence number lives alongside purely for display; it is never used as a key and never compared across nodes.
- **Version blobs live at paths derived from `key@versionID`**, so versions never collide with each other or the bare key.
- **The version index** is JSON at `<root>/<nodeID>/.vindex/<sha1(key)>.json`, written atomically: marshal to a `.tmp` file, then rename over the original. A crash mid-write cannot leave a torn index.
- **Locking is per-key**, not global: a `sync.Map` of `*sync.RWMutex` keyed by `nodeID:key`. Concurrent writes to different keys never contend.
- **`RollbackTo` appends**, not rewrites. It reads the old version and writes it as a new one, preserving the full history.
- **`MaxVersions`** prunes oldest-first when set; the default of 0 keeps everything forever.

### Encryption — `internal/crypto`

- **AES-256-CTR** via streaming decorators (`EncryptWriter`, `DecryptReader`) built on the standard library's `cipher.StreamWriter` / `cipher.StreamReader`. Nothing is buffered in memory; no encryption loop is hand-written.
- **A fresh 16-byte IV per blob**, from `crypto/rand`. Reusing an IV under one key is the one mistake CTR mode cannot survive — a test guards against it.
- **A 5-byte header** — `"WFS1"` plus a format version byte — before the IV. Without it, CTR would silently "decrypt" a pre-existing plaintext blob into garbage and report success. With it, the read fails with a clear error. The version byte leaves room to change the format later.
- **Optional and off by default.** `StoreOpts.Cipher` is an interface declared by `internal/store` and satisfied by `internal/crypto`; its zero value, `nil`, stores plaintext. All pre-existing storage tests pass unmodified.
- **Sizes are always plaintext sizes.** The file on disk is 21 bytes larger than what `Write` and `Read` report (the header and IV), and the two numbers are deliberately never conflated.
- **A per-node key** at `<DataDir>/encryption.key`, hex-encoded, mode `0600`, created on first use.

### Networking — `internal/node`

- **A `Node` wrapping a libp2p host.** TCP transport, Noise encryption on every connection, yamux stream multiplexing.
- **One stream per request.** Control messages and file transfers never share a pipe — no type-prefix byte, no read-loop pausing, no deadlock risk.
- **Persistent Ed25519 identity** at `<DataDir>/identity.key`, mode `0600`. This is load-bearing: `internal/store` scopes every path by the PeerID derived from this key. Regenerating it silently orphans every stored file.
- **mDNS discovery** on the LAN under a `"weavefs"` service tag (not libp2p's generic `_p2p._udp`). Best-effort: if multicast is unavailable the node logs a warning and keeps running.

### Wire protocol — `internal/proto`

Each request travels on its own libp2p stream, framed as a 4-byte big-endian length header, a JSON body, then the file bytes until the stream half-closes. One stream per request means no message-type prefix, no request identifiers, and no read-loop pausing — the machinery the original hand-written transport needed and got wrong.

### FileServer — `internal/server`

The only place `internal/node` and `internal/store` meet, which is what keeps either of them independently testable.

- **Write locally first, then replicate concurrently** on a best-effort basis.
- **Fetch checks local disk first**; otherwise tries peers one at a time until one answers, transferring exactly one copy.
- **`WriteResult`** reports both halves independently: `PeersTried`, `PeersStored`, and a `Failures` slice. A caller that wants to confirm a file is safely distributed must read here; the error return alone will not say so.
- **The `Authorizer` interface** receives the proven peer identity from libp2p's Noise handshake (never from the message body). It defaults to `AllowAll` and marks where a real policy goes.

### File sharing — the custodian vs. the handover

| | `put` (backup) | `send` (handover) |
|---|---|---|
| What crosses the wire | sender's **ciphertext** | **plaintext** (Noise encrypts the connection) |
| Recipient can read | no — never | yes — immediately |
| Recipient re-encrypts | no — not needed | yes — under their own key |
| Can be recalled | no — bytes go both ways | no — you gave it, it is theirs |

`send` uses a distinct protocol `/weavefs/share/1.0.0`. The sender reads through `ReadVersion` (decrypts) and streams plaintext; the recipient writes through `WriteVersion` (encrypts under its own key). The received file is a normal version the recipient owns — readable, removable, and version-tracked — exactly like anything it stored itself. If it collides with a key the recipient already has, it lands as a new version, by design.

**Verified end to end:** A sends a file to B, B gets it back byte-for-byte, and B's on-disk copy carries B's own `WFS1` header — proving it was re-encrypted under B's key, not merely relayed.

### Client API — `internal/api`

`serve` also runs a small HTTP server on `127.0.0.1` and writes the port to `<DataDir>/api.addr`. The `put`, `get`, `ls`, `rm`, and `send` client commands read that file and connect to the real running node — the one with its real peers and its live encryption key.

Loopback-only is deliberate. weaveFS has no access-control policy yet, so an API reachable from the network would be an unauthenticated remote handle on somebody's disk.

---

## Commands

```text
# Node commands
weavefs demo                           two nodes in one process — full round-trip proof
weavefs serve -data DIR                run a node until interrupted
weavefs id    -data DIR                print this node's PeerID and dialable addresses

# Client commands (talk to a running serve)
weavefs put  -data DIR KEY FILE        store a file; peers keep an unreadable custodian copy
weavefs send -data DIR TARGET KEY      give a peer a readable copy it will own outright
weavefs get  -data DIR KEY [OUT]       fetch a file (stdout if OUT is omitted)
weavefs ls   -data DIR KEY             list every version of a key
weavefs rm   -data DIR KEY             delete a key's local copies
```

**`serve` flags:** `-listen ADDR`, `-peer ADDR` (repeatable), `-api ADDR` (default `127.0.0.1:0`), `-no-mdns`.

**`put` flags:** `-m MESSAGE` (version message).

**`get` flags:** `-version UUID` (fetch a specific version rather than the latest).

**`send` target:** exactly one of `-peer PID` (repeatable), `-n COUNT`, or `-all`. Also accepts `-m`, `-version`, and `-as KEY` (the key the recipient files it under).

---

## The demo

`make run` runs the built-in demo: two nodes in one process, one stores and replicates, the other is shown to hold ciphertext, the first deletes its local copy, then recovers it.

```
-- 2. node A stores a file and replicates it to every connected peer ------
[server] replicated "quarterly_report" to 12D3KooWNE4s...
  wrote 48 bytes as version 0fa0b2af-d800-4585-bf34-2c0281693848 (seq 1)

-- 3. what node B now holds ------------------------------------------------
  filed under A's PeerID, inside B's directory:
    weavefs_data_b\12D3KooWKvMN...\6fe23\5ecb2\...\6fe235ecb2...
  hex     : 574653310142532a9e9d745ef85806951ad4e2eead71ef6301ecb9cf7c3d6b53...
  starts with the WFS1 blob header: true
  contains the plaintext           : false

-- 4. node A loses its local copy ------------------------------------------
  A still has it: false

-- 5. node A fetches the file back from node B -----------------------------
  recovered 48 bytes: "weaveFS: distributed storage, one node at a time"
  identical to the original: true
```

The file is 48 bytes; 69 bytes cross the wire. The difference is the 21-byte header and IV — two numbers deliberately never conflated.

---

## Build, test, run

```bash
make build   # go build -o bin/fs.exe ./cmd/weavefs
make run     # build, then run the demo
make test    # go test ./... -v
make install # install weavefs into $GOPATH/bin
```

A single test:

```bash
go test ./internal/store/ -run TestRollbackTo -v
```

---

## Development history

### Week 1 — hand-written TCP transport *(since deleted)*

The first week built a `p2p/` package from scratch: `Peer` and `Transport` interfaces, a 1-byte message-type prefix, a `HandshakeFunc` placeholder, and a `TCPTransport` that paused its read loop with `sync.WaitGroup` during file transfers.

### Weeks 2–3 — content-addressable storage

`internal/store` with `CASPathTransformFunc`, `Write`/`Read`/`Has`/`Delete`/`Clear`, then versioning on top: `WriteVersion`/`ReadVersion`/`ListVersions`/`DeleteVersion`/`RollbackTo`. 17 committed tests.

### Segment A — libp2p networking

Hand-written transport replaced with go-libp2p. One stream per request, persistent Ed25519 identity, mDNS discovery, Noise on every connection. The correctness class of bugs that plagued `p2p/` became structurally impossible.

### Segment B — encryption at rest

`internal/crypto` closes the storage side. AES-256-CTR streaming codec, fresh IV per blob, `WFS1` header for format detection, per-node key, optional via interface. 18 committed tests.

### Segments C and D — node-to-node transfer

`internal/proto` and `internal/server`. The system first did something a single-machine program cannot: store on one node, delete, recover from a peer. Replication, custodian recovery, and a real `demo` subcommand proving the full round trip end to end.

### Client API

`internal/api`. `serve` now runs a loopback HTTP server; `put`/`get`/`ls`/`rm` connect to the real running node. `WriteResult` gives callers honest replication outcomes rather than silently discarding them.

### File sharing

`/weavefs/share/1.0.0` protocol. `send` gives a peer a readable copy — decrypt on the way out, re-encrypt under the recipient's own key on the way in. Three targeting modes: one peer, N peers, all peers. Both stored keys and loose files off disk. Verified byte-for-byte across two real processes.

---

## What's not built yet

- **Replication repair.** `put` tells you where a file went at the moment of writing. If a custodian dies later, nothing notices. The write-time reporting is done (item 10, piece 1); the background repair loop is not (pieces 2 and 3).
- **Access control.** Any node on the LAN may store files here and request them. The `Authorizer` interface marks where a real policy slots in — item 02.
- **Remote administration.** The client API binds to loopback only. Remote management is deferred until there is a policy worth enforcing.
- **Integrity protection.** AES-256-CTR provides confidentiality, not integrity. A tampered blob decrypts to wrong plaintext silently. Adding AEAD (AES-GCM or ChaCha20-Poly1305) is the obvious next step — item 03.
- **Replication placement.** `put` sends to every peer with no capacity or failure-domain awareness — item 06.

---

## Package summary

| Package | What it does | Committed tests |
|---|---|---|
| `internal/store` | Content-addressable storage, immutable versioning, per-key locking, optional encryption, raw blob paths for replication | 17 |
| `internal/crypto` | AES-256-CTR encryption at rest, per-node key management | 18 (1 skipped on Windows) |
| `internal/fsutil` | Atomic file replacement, shared by the two key-file writers | 6 (1 skipped on Windows) |
| `internal/node` | libp2p host: persistent identity, Noise-encrypted connections, mDNS discovery | 0 — see below |
| `internal/proto` | The request/response messages nodes exchange, and the frame codec | 0 — see below |
| `internal/server` | `FileServer` joining networking to storage: replication, recovery, readable file sharing, per-peer outcome reporting | 0 — see below |
| `internal/api` | Loopback HTTP API so another process can drive a running node | 0 — see below |
| `cmd/weavefs` | `demo`, `serve`, `id`, and the client commands `put`, `send`, `get`, `ls`, `rm` | — |

> **On the empty test columns.** Tests were written and run for all four of those packages — 54 of them, all passing — and then deleted before each commit, because the project's working practice is that test files are used to verify a change and do not enter the repository. The most significant consequence is that there is no regression guard against regenerating `identity.key`, which silently orphans every stored file.

---

## License

MIT. Built by [@Sambodhi-Roy](https://github.com/Sambodhi-Roy).
