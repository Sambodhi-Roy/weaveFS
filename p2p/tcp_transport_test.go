package p2p

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTCPTransportListenAndAccept(t *testing.T) {
	opts := TCPTransportOpts{
		ListenAddr:    ":3000",
		HandshakeFunc: NOPHandshakeFunc,
		Decoder:       DefaultDecoder{},
	}

	tr := NewTCPTransport(opts)
	assert.Equal(t, ":3000", tr.Addr())

	err := tr.ListenAndAccept()
	assert.Nil(t, err)

	tr.Close()
}
