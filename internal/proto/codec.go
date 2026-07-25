package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// lengthPrefixSize is the width of the frame's length field. A uint32 caps a
// header at four gibibytes, far more than maxHeaderBytes allows, so the field
// itself will never be the limiting factor.
const lengthPrefixSize = 4

// maxHeaderBytes bounds how large a header a peer may claim to be sending.
//
// Without a cap, a hostile or broken peer sends a length of 0xFFFFFFFF and this
// process tries to allocate four gigabytes on their say-so. One mebibyte is
// several thousand times larger than any real message here and still trivially
// cheap to refuse.
const maxHeaderBytes = 1 << 20

// WriteMessage encodes v as JSON and writes it to w with a length prefix.
//
// The parameter type "any" is Go's empty interface, satisfied by every type —
// the rough equivalent of Object in Java or C#. Callers pass a concrete message
// struct and encoding/json reflects over it.
func WriteMessage(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: WriteMessage: encoding %T: %w", v, err)
	}

	if len(payload) > maxHeaderBytes {
		return fmt.Errorf("proto: WriteMessage: %T encodes to %d bytes, over the %d byte limit",
			v, len(payload), maxHeaderBytes)
	}

	// The length and the payload go out as one Write rather than two. Two
	// writes are two packets on the wire for no benefit, and the reference
	// implementation's split between its type byte and its payload is a
	// non-atomic pair that can interleave with a concurrent writer.
	frame := make([]byte, lengthPrefixSize+len(payload))
	binary.BigEndian.PutUint32(frame[:lengthPrefixSize], uint32(len(payload)))
	copy(frame[lengthPrefixSize:], payload)

	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("proto: WriteMessage: writing %T: %w", v, err)
	}
	return nil
}

// ReadMessage reads one length-prefixed JSON message from r and decodes it into
// v, which must be a pointer.
//
// Both reads use io.ReadFull rather than a bare r.Read, and this is the single
// most important line in the package. Read is permitted to return fewer bytes
// than asked for — it returns whatever has arrived so far — so a message split
// across two network packets arrives in two pieces. The hand-written transport
// this replaces called Read once, treated a partial read as the whole message,
// and silently truncated anything that did not fit in one packet. ReadFull
// loops until the buffer is full or the stream genuinely ends.
func ReadMessage(r io.Reader, v any) error {
	var prefix [lengthPrefixSize]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return fmt.Errorf("proto: ReadMessage: reading length prefix: %w", err)
	}

	length := binary.BigEndian.Uint32(prefix[:])
	if length > maxHeaderBytes {
		return fmt.Errorf("proto: ReadMessage: peer announced a %d byte header, over the %d byte limit",
			length, maxHeaderBytes)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("proto: ReadMessage: reading %d byte header: %w", length, err)
	}

	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("proto: ReadMessage: decoding into %T: %w", v, err)
	}
	return nil
}
