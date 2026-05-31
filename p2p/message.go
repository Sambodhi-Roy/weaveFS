package p2p

// RPC holds any message received from a remote peer.
type RPC struct {
	From    string
	Payload []byte
	Stream  bool
}
