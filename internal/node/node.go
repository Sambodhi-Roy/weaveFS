// Package node runs this machine's presence on the weaveFS network.
//
// It owns the libp2p host: the node's cryptographic identity, the addresses it
// listens on, the encrypted connections it holds open, and the local-network
// discovery that finds other nodes. It deliberately knows nothing about files,
// storage or versioning — internal/server composes this package with
// internal/store to build a working file server.
package node

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// defaultListenAddr binds every interface on an OS-assigned port. Port 0 keeps
// tests free of "address already in use" flakes, and binding all interfaces is
// what local-network discovery needs to be reachable.
const defaultListenAddr = "/ip4/0.0.0.0/tcp/0"

// defaultServiceName isolates weaveFS discovery from other libp2p software.
// The mdns package default ("_p2p._udp") is the generic libp2p tag, which would
// also surface unrelated nodes such as a developer's IPFS daemon.
const defaultServiceName = "weavefs"

// Config describes how a node joins the network.
type Config struct {
	// ListenAddrs are multiaddresses to listen on, e.g. "/ip4/0.0.0.0/tcp/4001".
	// Defaults to defaultListenAddr.
	ListenAddrs []string

	// DataDir holds the node's identity key. It is the same directory
	// internal/store uses as its Root, so one node is one directory.
	DataDir string

	// ServiceName is the discovery tag. Only nodes sharing this string find
	// each other, so separate networks can coexist on one LAN.
	// Defaults to defaultServiceName.
	ServiceName string

	// DisableMDNS turns off local-network discovery. The zero value keeps
	// discovery on, which is what a normal node wants.
	DisableMDNS bool
}

// withDefaults returns a copy of the config with empty fields filled in.
func (c Config) withDefaults() Config {
	if len(c.ListenAddrs) == 0 {
		c.ListenAddrs = []string{defaultListenAddr}
	}
	if c.ServiceName == "" {
		c.ServiceName = defaultServiceName
	}
	return c
}

// Node is this machine's presence on the weaveFS network.
type Node struct {
	host host.Host

	// mdns is nil when discovery is disabled or failed to start.
	mdns mdns.Service

	// closeOnce makes Close safe to call more than once, which matters when a
	// caller closes on an error path and again from a defer.
	closeOnce sync.Once
	closeErr  error
}

// New starts a libp2p host using the identity stored in cfg.DataDir, creating
// that identity on first use.
//
// The returned Node is listening and, unless discovery is disabled, already
// looking for peers. Callers must Close it.
func New(ctx context.Context, cfg Config) (*Node, error) {
	cfg = cfg.withDefaults()

	if cfg.DataDir == "" {
		return nil, fmt.Errorf("node: DataDir is required")
	}

	priv, err := loadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	// libp2p's defaults for transport, security (Noise) and stream
	// multiplexing (yamux) are what we want; only identity and addresses
	// are ours to choose.
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("node: starting libp2p host: %w", err)
	}

	n := &Node{host: h}
	if !cfg.DisableMDNS {
		n.mdns = startDiscovery(ctx, h, cfg.ServiceName)
	}

	log.Printf("[node] started %s listening on %v", n.ID(), n.Addrs())
	return n, nil
}

// ID returns this node's PeerID, derived from its identity key and stable
// across restarts. internal/store uses it to scope every stored path.
func (n *Node) ID() string {
	return n.host.ID().String()
}

// Host exposes the underlying libp2p host so internal/server can register
// protocol handlers and open streams.
func (n *Node) Host() host.Host {
	return n.host
}

// Addrs returns the addresses this node actually bound, with the PeerID
// appended so each one is directly dialable by another node.
func (n *Node) Addrs() []string {
	info := peer.AddrInfo{ID: n.host.ID(), Addrs: n.host.Addrs()}

	multiaddrs, err := peer.AddrInfoToP2pAddrs(&info)
	if err != nil {
		log.Printf("[node] could not format listen addresses: %v", err)
		return nil
	}

	out := make([]string, 0, len(multiaddrs))
	for _, addr := range multiaddrs {
		out = append(out, addr.String())
	}
	return out
}

// AddrInfo returns the identity and addresses another node needs in order to
// connect to this one.
func (n *Node) AddrInfo() peer.AddrInfo {
	return peer.AddrInfo{ID: n.host.ID(), Addrs: n.host.Addrs()}
}

// Peers returns the nodes currently connected.
func (n *Node) Peers() []peer.ID {
	return n.host.Network().Peers()
}

// DiscoveryActive reports whether local-network discovery is running. It is
// false when discovery was disabled or when multicast was unavailable, in
// which case peers must be connected explicitly.
func (n *Node) DiscoveryActive() bool {
	return n.mdns != nil
}

// Close shuts down discovery and then the host. It is safe to call repeatedly.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		if n.mdns != nil {
			if err := n.mdns.Close(); err != nil {
				log.Printf("[node] closing discovery: %v", err)
			}
		}
		n.closeErr = n.host.Close()
	})
	return n.closeErr
}
