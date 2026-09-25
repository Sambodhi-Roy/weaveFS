# weaveFS

<p align="center">
  <b>A distributed file system built from scratch in Go.</b>
</p>

<p align="center">
  Encryption • Versioning • Peer Replication • File Sharing
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25.7+-00ADD8?logo=go&logoColor=white">
  <img src="https://img.shields.io/badge/transport-libp2p-blueviolet">
  <img src="https://img.shields.io/badge/encryption-AES--256--CTR-green">
</p>

---

## What is weaveFS?

weaveFS is a small distributed file system built in Go to explore how storage, networking, encryption, and replication fit together.

Files can be replicated across peers without giving those peers access to the original file.

> **Peers store your ciphertext, not your file.**

If your local copy disappears, weaveFS can recover the encrypted copy from a peer and decrypt it locally.

---

## Features

* 🔐 **Encrypted storage** using AES-256-CTR
* 🕐 **File versioning** with immutable versions and rollback
* 🌐 **Peer-to-peer replication** using libp2p
* 📡 **Automatic LAN discovery** using mDNS
* 🤝 **File sharing** with `send`
* 🔑 **Per-node encryption keys and identities**
* 💻 **Simple CLI** for interacting with running nodes

---

## Quick Start

### Requirements

* Go 1.25.7+

### Install

```bash
git clone https://github.com/Sambodhi-Roy/weaveFS
cd weaveFS
make install
```

### Run two nodes

**Terminal 1**

```bash
weavefs serve -data node_a
```

**Terminal 2**

```bash
weavefs serve -data node_b -peer <NODE_A_ADDRESS>
```

Now store a file:

```bash
weavefs put -data node_a report ./report.pdf
```

Check its versions:

```bash
weavefs ls -data node_a report
```

Delete the local copy:

```bash
weavefs rm -data node_a report
```

Recover it from the peer:

```bash
weavefs get -data node_a report ./recovered.pdf
```

The peer only stored the encrypted copy. Node A decrypts it after recovery.

---

## Backup vs Sharing

weaveFS treats replication and file sharing differently.

|                                    | `put`  | `send`    |
| ---------------------------------- | ------ | --------- |
| Purpose                            | Backup | Share     |
| Recipient can read it              | No     | Yes       |
| Recipient stores it with their key | No     | Yes       |
| Ownership                          | You    | Recipient |

Share a file with one peer:

```bash
weavefs send -data node_a -peer <PEER_ID> report
```

Or with every connected peer:

```bash
weavefs send -data node_a -all report
```

---

## How it works

```text
                 ┌───────────┐
                 │    CLI    │
                 └─────┬─────┘
                       │
                 ┌─────▼─────┐
                 │    API    │
                 └─────┬─────┘
                       │
                ┌──────▼──────┐
                │ FileServer  │
                └──────┬──────┘
                       │
              ┌────────┴────────┐
              │                 │
        ┌─────▼─────┐     ┌─────▼─────┐
        │   Store   │     │    Node   │
        │ Versioning│     │   libp2p  │
        └─────┬─────┘     └───────────┘
              │
        ┌─────▼─────┐
        │   Crypto  │
        └───────────┘
```

The main components are:

```text
internal/store    Storage and versioning
internal/crypto   Encryption
internal/node     libp2p networking and discovery
internal/proto    Wire protocol
internal/server   Replication and file sharing
internal/api      Local client API
cmd/weavefs       CLI
```

---

## Commands

```bash
# Start a node
weavefs serve -data DIR

# Show node identity
weavefs id -data DIR

# Store a file
weavefs put -data DIR KEY FILE

# Share a file
weavefs send -data DIR TARGET KEY

# Retrieve a file
weavefs get -data DIR KEY [OUT]

# List versions
weavefs ls -data DIR KEY

# Remove local copies
weavefs rm -data DIR KEY

# Run the demo
weavefs demo
```

---

## Development

```bash
make build
make run
make test
make install
```

Run a specific test:

```bash
go test ./internal/store/ -run TestRollbackTo -v
```

---

## What's next?

* 🔄 Replication repair
* 🔒 Access control
* 🛡️ Authenticated encryption
* 🌍 Remote administration
* 📦 Smarter replication and failure-domain awareness

---

## Why weaveFS?

This project is being built incrementally to understand distributed systems by actually building one.

From a simple TCP transport to libp2p, from basic storage to versioning, and from local files to encrypted peer replication, each layer is built and tested before moving to the next.

**One node at a time.**
