package p2p

import (
	"encoding/gob"
	"io"
)

// Decoder defines the interface for decoding an incoming RPC from a reader.
type Decoder interface {
	Decode(io.Reader, *RPC) error
}

// GOBDecoder uses Go's built-in gob encoding.
type GOBDecoder struct{}

func (dec GOBDecoder) Decode(r io.Reader, msg *RPC) error {
	return gob.NewDecoder(r).Decode(msg)
}

// DefaultDecoder reads a 1-byte type prefix to determine if the incoming
// data is a regular message or a raw file stream.
type DefaultDecoder struct{}

func (dec DefaultDecoder) Decode(r io.Reader, msg *RPC) error {
	peekBuf := make([]byte, 1)
	if _, err := r.Read(peekBuf); err != nil {
		return err
	}

	// If it's a stream, just flag it — don't read the rest.
	// The file server will handle the raw bytes directly.
	if peekBuf[0] == IncomingStream {
		msg.Stream = true
		return nil
	}

	buf := make([]byte, 1028)
	n, err := r.Read(buf)
	if err != nil {
		return err
	}

	msg.Payload = buf[:n]
	return nil
}
