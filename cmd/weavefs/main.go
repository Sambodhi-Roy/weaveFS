// Command weavefs runs a weaveFS node and talks to one that is already running.
//
// The subcommands fall into two groups.
//
// These start a node:
//
//	weavefs demo              two nodes in one process, proving a full round trip
//	weavefs serve  -data DIR  run a node until interrupted
//	weavefs id     -data DIR  print this node's identity and addresses
//
// These talk to a node that "serve" is already running:
//
//	weavefs put  -data DIR <key> <file>              store a file
//	weavefs send -data DIR <target> <key> [file]     give a peer a readable copy
//	weavefs get  -data DIR <key> [outfile]           fetch a file
//	weavefs ls   -data DIR <key>                      list a key's versions
//	weavefs rm   -data DIR <key>                      drop a key's local copies
//
// put and send are the two ways to move a file to another machine, and they are
// deliberately different. put keeps the file for this node and scatters
// unreadable custodian copies to peers — a backup. send hands a chosen peer a
// readable copy it will own — a handover, and one that cannot be recalled.
//
// The second group does not start a node of its own, and that distinction is
// the reason those commands took so long to arrive. A "put" that quietly
// started a throwaway node against the same data directory would be a second
// process holding the same identity.key and blob tree, with no peer
// connections, since discovery takes longer than such a process lives. It would
// print "wrote 47 bytes" having replicated to nobody.
//
// So "serve" opens a small HTTP API on 127.0.0.1 and writes the port it got to
// <DataDir>/api.addr. The client commands read that file and connect, which
// means the node doing the work is the real one, with its real peers. The
// listener is loopback-only because weaveFS has no access-control policy yet;
// see internal/api for the full reasoning.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Sambodhi-Roy/weaveFS/internal/api"
	"github.com/Sambodhi-Roy/weaveFS/internal/crypto"
	"github.com/Sambodhi-Roy/weaveFS/internal/node"
	"github.com/Sambodhi-Roy/weaveFS/internal/server"
	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

func main() {
	// Logs carry their own [component] prefixes, so the default timestamp adds
	// noise without adding information.
	log.SetFlags(0)

	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	var err error
	switch command {
	case "demo":
		err = runDemo()
	case "serve":
		err = runServe(os.Args[2:])
	case "id":
		err = runID(os.Args[2:])
	case "put":
		err = runPut(os.Args[2:])
	case "send":
		err = runSend(os.Args[2:])
	case "get":
		err = runGet(os.Args[2:])
	case "ls":
		err = runList(os.Args[2:])
	case "rm":
		err = runRemove(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		log.Fatalf("\nunknown command %q", command)
	}

	if err != nil {
		log.Fatalf("weavefs %s: %v", command, err)
	}
}

func usage() {
	fmt.Print(`weaveFS — a distributed file system

Running a node:
  weavefs demo                     run two nodes in one process and prove a round trip
  weavefs serve -data DIR          run a node until interrupted
  weavefs id    -data DIR          print this node's PeerID and addresses

Using a node that "serve" is already running:
  weavefs put  -data DIR KEY FILE       store a file (peers keep an unreadable copy)
  weavefs send -data DIR TARGET KEY [FILE]  give a peer a readable copy it will own
  weavefs get  -data DIR KEY [OUT]      fetch a file (stdout if OUT is omitted)
  weavefs ls   -data DIR KEY            list every version of a key
  weavefs rm   -data DIR KEY            delete a key's local copies

put vs send:
  put   keeps the file for you; peers hold ciphertext only you can read — a backup.
  send  hands a chosen peer a readable copy it owns — a handover, and final.

Flags for serve:
  -data DIR        node directory, holding identity.key, encryption.key and blobs
  -listen ADDR     multiaddr to listen on (default /ip4/0.0.0.0/tcp/0)
  -peer ADDR       a peer to dial on startup; may be repeated
  -api ADDR        address for the local client API (default 127.0.0.1:0)
  -no-mdns         disable local-network discovery

Targeting a send (choose exactly one):
  -peer PID        a specific peer's PeerID; may be repeated for several peers
  -n COUNT         any COUNT of the connected peers
  -all             every connected peer

Flags for the client commands:
  -data DIR        the directory of the running node to talk to
  -m MESSAGE       (put, send) a note describing this version, like a commit message
  -version ID      (get, send) one specific version instead of the latest
  -as KEY          (send) the key the recipient files it under (default: same key)

Getting started — three terminals:
  1  weavefs serve -data node_a
  2  weavefs serve -data node_b -peer <the multiaddr terminal 1 printed>
  3  weavefs put -data node_a report ./notes.txt
     weavefs rm  -data node_a report
     weavefs get -data node_a report recovered.txt
`)
}

// weaveNode is one running node: a libp2p host, a store, and the file server
// that joins them.
type weaveNode struct {
	node   *node.Node
	store  *store.Store
	server *server.FileServer
	dir    string
}

// startNode builds a complete node in dataDir, creating its identity and
// encryption keys on first use.
//
// The store's Root is the same directory as the node's DataDir, so one node is
// one directory: identity.key, encryption.key and every blob together. That is
// also why the identity key sits beside the per-node folders rather than inside
// one — the folder is named after the PeerID, which you need the key to compute.
func startNode(ctx context.Context, dataDir string, listenAddrs []string, disableMDNS bool) (*weaveNode, error) {
	n, err := node.New(ctx, node.Config{
		ListenAddrs: listenAddrs,
		DataDir:     dataDir,
		DisableMDNS: disableMDNS,
	})
	if err != nil {
		return nil, err
	}

	key, err := crypto.LoadOrCreateKey(dataDir)
	if err != nil {
		n.Close()
		return nil, err
	}

	cipher, err := crypto.NewAESCipher(key)
	if err != nil {
		n.Close()
		return nil, err
	}

	st := store.NewStore(store.StoreOpts{
		Root:              dataDir,
		PathTransformFunc: store.CASPathTransformFunc,
		Cipher:            cipher,
	})

	srv, err := server.New(server.Config{Node: n, Store: st})
	if err != nil {
		n.Close()
		return nil, err
	}
	if err := srv.Start(); err != nil {
		n.Close()
		return nil, err
	}

	return &weaveNode{node: n, store: st, server: srv, dir: dataDir}, nil
}

// close shuts the node down. The server is closed first so no new request is
// accepted while the host is going away.
func (w *weaveNode) close() {
	_ = w.server.Close()
	_ = w.node.Close()
}

// ---------------------------------------------------------------- demo

const (
	demoDirA = "weavefs_data_a"
	demoDirB = "weavefs_data_b"
	demoKey  = "quarterly_report"
)

var demoPayload = []byte("weaveFS: distributed storage, one node at a time")

// runDemo proves the whole segment end to end, in one process.
//
// Node A stores a file and replicates it to node B. B holds ciphertext it has
// no key for. A then deletes its own copy entirely and fetches the file back
// from B, recovering the original bytes.
func runDemo() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start from clean directories so the demo shows a first run every time,
	// rather than reporting a file left over from the last one.
	for _, dir := range []string{demoDirA, demoDirB} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("clearing %s: %w", dir, err)
		}
	}

	// Discovery is switched off and the two nodes are introduced explicitly.
	// Multicast is unavailable in plenty of environments, and a demo that
	// sometimes hangs is worse than one that always works.
	local := []string{"/ip4/127.0.0.1/tcp/0"}

	a, err := startNode(ctx, demoDirA, local, true)
	if err != nil {
		return fmt.Errorf("starting node A: %w", err)
	}
	defer a.close()

	b, err := startNode(ctx, demoDirB, local, true)
	if err != nil {
		return fmt.Errorf("starting node B: %w", err)
	}
	defer b.close()

	step(1, "connecting the two nodes")
	if err := a.node.Host().Connect(ctx, b.node.AddrInfo()); err != nil {
		return fmt.Errorf("connecting A to B: %w", err)
	}
	fmt.Printf("  A is %s\n  B is %s\n", a.server.ID(), b.server.ID())

	step(2, "node A stores a file, and replicates it to every connected peer")
	result, err := a.server.StoreVersion(ctx, demoKey, "first draft", bytes.NewReader(demoPayload))
	if err != nil {
		return fmt.Errorf("storing on A: %w", err)
	}
	fmt.Printf("  wrote %d bytes as version %s (seq %d)\n",
		result.Entry.SizeBytes, result.Entry.VersionID, result.Entry.Seq)
	fmt.Printf("  replicated to %d of %d peer(s)\n", result.PeersStored, result.PeersTried)
	for _, f := range result.Failures {
		fmt.Printf("  %s did not take a copy: %v\n", f.Peer, f.Err)
	}

	step(3, "what node B now holds")
	if !b.store.Has(a.server.ID(), demoKey) {
		return fmt.Errorf("replication did not reach node B")
	}
	if err := showCustodianBlob(b.dir, a.server.ID()); err != nil {
		return err
	}

	step(4, "node A loses its local copy")
	if err := a.store.Delete(a.server.ID(), demoKey); err != nil {
		return fmt.Errorf("deleting A's copy: %w", err)
	}
	fmt.Printf("  A still has it: %v\n", a.store.Has(a.server.ID(), demoKey))

	step(5, "node A fetches the file back from node B")
	size, rc, err := a.server.Get(ctx, demoKey)
	if err != nil {
		return fmt.Errorf("fetching from the network: %w", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("reading the recovered file: %w", err)
	}

	fmt.Printf("  recovered %d bytes: %q\n", size, string(got))
	fmt.Printf("  identical to the original: %v\n", bytes.Equal(demoPayload, got))

	if !bytes.Equal(demoPayload, got) {
		return fmt.Errorf("the recovered file does not match the original")
	}

	fmt.Printf("\nBoth directories are left in place for inspection:\n  %s\n  %s\n",
		demoDirA, demoDirB)
	return nil
}

// step prints a numbered heading, so the output reads as a narrative rather
// than a wall of log lines.
func step(n int, title string) {
	fmt.Printf("\n── %d. %s ─────────────────────────────\n", n, title)
}

// showCustodianBlob prints the first bytes of the replica sitting on the
// custodian's disk, to demonstrate rather than merely assert that it is
// unreadable.
func showCustodianBlob(custodianDir, originID string) error {
	path, err := firstBlobPath(filepath.Join(custodianDir, originID))
	if err != nil {
		return err
	}

	raw, err := readAtMost(path, 48)
	if err != nil {
		return err
	}

	fmt.Printf("  filed under A's PeerID, inside B's directory:\n    %s\n", path)
	fmt.Printf("  hex     : %s\n", hex.EncodeToString(raw))
	fmt.Printf("  as text : %q\n", sanitise(raw))
	fmt.Printf("  starts with the WFS1 blob header: %v\n", bytes.HasPrefix(raw, []byte("WFS1")))
	fmt.Printf("  contains the plaintext           : %v\n", bytes.Contains(raw, demoPayload))
	return nil
}

// firstBlobPath returns any blob file beneath dir, skipping the .vindex
// directory that holds version metadata rather than content.
func firstBlobPath(dir string) (string, error) {
	var found string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".vindex" {
				return fs.SkipDir
			}
			return nil
		}
		found = path
		return fs.SkipAll
	})
	if err != nil {
		return "", fmt.Errorf("looking for a blob under %s: %w", dir, err)
	}
	if found == "" {
		return "", fmt.Errorf("no blob found under %s", dir)
	}
	return found, nil
}

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

// ---------------------------------------------------------------- serve

// repeatedFlag collects a flag that may appear more than once, so several
// -peer addresses can be given on one command line.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ", ") }

func (r *repeatedFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data", "weavefs_data", "node directory")
	listen := fs.String("listen", "/ip4/0.0.0.0/tcp/0", "multiaddr to listen on")
	apiAddr := fs.String("api", api.DefaultAddr, "address for the local client API")
	noMDNS := fs.Bool("no-mdns", false, "disable local-network discovery")

	var peers repeatedFlag
	fs.Var(&peers, "peer", "a peer multiaddr to dial on startup; may be repeated")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// The context is cancelled on Ctrl-C, which unblocks the wait below and
	// runs the deferred shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w, err := startNode(ctx, *dataDir, []string{*listen}, *noMDNS)
	if err != nil {
		return err
	}
	defer w.close()

	// The client API is what makes put, get, ls and rm possible: it is the only
	// way another process can reach this node. Started after the node so that a
	// port is never published for something that failed to come up.
	clientAPI, err := api.New(api.Config{
		FileServer: w.server,
		DataDir:    *dataDir,
		Addr:       *apiAddr,
	})
	if err != nil {
		return err
	}
	if err := clientAPI.Start(); err != nil {
		return err
	}
	defer clientAPI.Close()

	for _, addr := range peers {
		if err := dialPeer(ctx, w, addr); err != nil {
			log.Printf("[main] could not dial %s: %v", addr, err)
		}
	}

	if w.node.DiscoveryActive() {
		log.Printf("[main] discovering peers on the local network")
	} else {
		log.Printf("[main] discovery is off; peers must be given with -peer")
	}

	fmt.Println("\nnode is running. Addresses another node can dial:")
	for _, addr := range w.node.Addrs() {
		fmt.Printf("  %s\n", addr)
	}

	fmt.Printf("\nclient API: http://%s\n", clientAPI.Addr())
	fmt.Printf("from another terminal:\n")
	fmt.Printf("  weavefs put -data %s <key> <file>\n", *dataDir)
	fmt.Printf("  weavefs get -data %s <key> [outfile]\n", *dataDir)

	fmt.Println("\npress Ctrl-C to stop")

	<-ctx.Done()
	fmt.Println("\nshutting down")
	return nil
}

// dialPeer parses a full multiaddr including its /p2p/<PeerID> suffix and
// connects to it.
func dialPeer(ctx context.Context, w *weaveNode, addr string) error {
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return fmt.Errorf("not a valid multiaddr: %w", err)
	}

	info, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return fmt.Errorf("address has no /p2p/<PeerID> suffix: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := w.node.Host().Connect(dialCtx, *info); err != nil {
		return err
	}

	log.Printf("[main] connected to %s", info.ID)
	return nil
}

// ---------------------------------------------------------------- id

func runID(args []string) error {
	fs := flag.NewFlagSet("id", flag.ExitOnError)
	dataDir := fs.String("data", "weavefs_data", "node directory")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Discovery stays off: this only reports the identity and exits, and there
	// is no reason to announce ourselves on the network to do that.
	w, err := startNode(ctx, *dataDir, []string{"/ip4/0.0.0.0/tcp/0"}, true)
	if err != nil {
		return err
	}
	defer w.close()

	fmt.Printf("PeerID   : %s\n", w.server.ID())
	fmt.Printf("directory: %s\n", *dataDir)
	fmt.Println("addresses:")
	for _, addr := range w.node.Addrs() {
		fmt.Printf("  %s\n", addr)
	}
	return nil
}
