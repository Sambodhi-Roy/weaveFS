package p2p

const (
	IncomingMessage = 0x1
	IncomingStream  = 0x2
)

// RPC holds any message received from a remote peer.
type RPC struct {
	From    string
	Payload []byte
	Stream  bool
}
