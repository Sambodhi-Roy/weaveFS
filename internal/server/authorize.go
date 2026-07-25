package server

import "github.com/libp2p/go-libp2p/core/peer"

// Authorizer decides whether a peer may perform an operation on this node.
//
// The peer identity handed to these methods comes from libp2p's Noise
// handshake, which proves cryptographically which private key is on the other
// end of the connection. It is never read out of the message body, because a
// PeerID written into JSON is a claim rather than a proof, and a claim sitting
// next to a proof is an invitation to trust the wrong one.
//
// Returning an error refuses the operation. The requester is told it was
// refused; it is not told why, since the reason is this node's business.
type Authorizer interface {
	// AllowStore decides whether `from` may place a blob on this node, filed
	// under originID's namespace.
	AllowStore(from peer.ID, originID, key string) error

	// AllowGet decides whether `from` may retrieve a blob this node is holding
	// under originID's namespace.
	AllowGet(from peer.ID, originID, key string) error
}

// AllowAll permits every operation from every peer, and is what a FileServer
// uses when no Authorizer is configured.
//
// This is the honest default for where weaveFS currently is, not a
// recommendation. Any node on the local network can store files on this one and
// ask it for files, which is fine on a trusted development network and is not
// fine anywhere else. Two concrete exposures follow from it: an unauthorised
// peer can fill this node's disk, since nothing imposes a quota; and an
// unauthorised peer can ask for any blob by key.
//
// Closing that gap is deferred work, tracked in
// improvements -for-future/02-connection-gating.md. This interface exists so
// that closing it later changes no wire format and no message type — the
// identity a real policy needs is already available and already proven.
type AllowAll struct{}

// AllowStore permits every store request.
func (AllowAll) AllowStore(peer.ID, string, string) error { return nil }

// AllowGet permits every get request.
func (AllowAll) AllowGet(peer.ID, string, string) error { return nil }
