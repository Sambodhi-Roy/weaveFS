package p2p

// HandshakeFunc is called after a connection is established.
// It can be used to verify peer identity, exchange keys, etc.
type HandshakeFunc func(Peer) error

// NOPHandshakeFunc is a no-op handshake — accepts all peers unconditionally.
func NOPHandshakeFunc(Peer) error { return nil }
