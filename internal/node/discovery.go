package node

import (
	"context"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// connectTimeout bounds a single dial attempt to a newly discovered peer, so a
// silent or unreachable host cannot tie up the discovery callback.
const connectTimeout = 10 * time.Second

// discoveryNotifee reacts to peers found on the local network by connecting to
// them. It satisfies mdns.Notifee.
type discoveryNotifee struct {
	host host.Host
	ctx  context.Context
}

// HandlePeerFound is called by the mDNS service for every peer it hears from,
// including this node itself — mDNS receives its own announcements, so the
// self-check is required rather than defensive.
func (d *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == d.host.ID() {
		return
	}

	ctx, cancel := context.WithTimeout(d.ctx, connectTimeout)
	defer cancel()

	if err := d.host.Connect(ctx, pi); err != nil {
		log.Printf("[node] could not connect to discovered peer %s: %v", pi.ID, err)
		return
	}
	log.Printf("[node] connected to discovered peer %s", pi.ID)
}

// startDiscovery begins announcing this node on the local network and
// connecting to the weaveFS nodes it hears back from.
//
// Discovery is best-effort. Multicast is blocked in many CI runners, container
// networks and sandboxes, and a node without discovery is still fully usable —
// peers can be connected explicitly. A failure here is therefore logged loudly
// and reported to the caller as a nil service rather than an error that would
// prevent the node from starting at all.
func startDiscovery(ctx context.Context, h host.Host, serviceName string) mdns.Service {
	notifee := &discoveryNotifee{host: h, ctx: ctx}
	service := mdns.NewMdnsService(h, serviceName, notifee)

	if err := service.Start(); err != nil {
		log.Printf("[node] WARNING: local network discovery unavailable: %v", err)
		log.Printf("[node] WARNING: this node will only see peers it connects to explicitly")
		return nil
	}

	log.Printf("[node] discovering peers on the local network as %q", serviceName)
	return service
}
