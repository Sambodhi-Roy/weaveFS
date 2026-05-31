package p2p

import "net"

// Peer represents a remote node in the network.
// It embeds net.Conn so it can be used like any standard connection.
type Peer interface {
	net.Conn
	Send([]byte) error
	CloseStream()
}

// Transport handles communication between nodes.
// This could be TCP, UDP, WebSockets, etc.
type Transport interface {
	Addr() string
	Dial(string) error
	ListenAndAccept() error
	Consume() <-chan RPC
	Close() error
}
