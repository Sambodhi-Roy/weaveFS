package p2p

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTCPTransportListenAndAccept(t *testing.T) {
	opts := TCPTransportOpts{
		// Use :0 so the OS picks a free port — avoids flaky "address in use" errors.
		ListenAddr:    ":0",
		HandshakeFunc: NOPHandshakeFunc,
		Decoder:       DefaultDecoder{},
	}

	tr := NewTCPTransport(opts)

	err := tr.ListenAndAccept()
	require.NoError(t, err, "ListenAndAccept should succeed on an OS-assigned port")

	// Only close if we successfully started listening.
	defer tr.Close()
}

