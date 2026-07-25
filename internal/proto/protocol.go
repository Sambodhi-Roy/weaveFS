// Package proto defines the language weaveFS nodes speak to each other.
//
// It is deliberately inert: types, a frame encoder and a frame decoder, and
// nothing that opens a connection or touches a disk. internal/server imports
// this package and does the talking. That separation means the wire format can
// be tested exhaustively against an in-memory buffer, with no network involved.
//
// # How a request travels
//
// Every request gets its own libp2p stream — an independent, ordered, two-way
// channel. Many streams share one underlying connection and the multiplexer
// keeps them apart, so a control message and a multi-gigabyte file transfer
// never contend for the same pipe.
//
// That single fact removes most of what a hand-written protocol needs. There is
// no message-type prefix byte, because the protocol ID already routed the
// stream to the right handler. There is no length field on the file body,
// because closing the write half of the stream produces a real end-of-file.
// And there is no request identifier, because a response can only arrive on the
// stream that carried its request.
//
// # The frame
//
//	┌───────────────┬──────────────────────┬───────────────────────────┐
//	│ length        │ JSON header          │ raw blob bytes            │
//	│ 4 bytes,      │ exactly `length`     │ until the stream ends     │
//	│ big-endian    │ bytes of JSON        │ (absent on most messages) │
//	└───────────────┴──────────────────────┴───────────────────────────┘
//
// Blob bytes, where present, are the file exactly as it sits on the sender's
// disk — still encrypted, header and all. A peer storing someone else's blob is
// a custodian: it writes the bytes down without ever being able to read them.
package proto

import "github.com/libp2p/go-libp2p/core/protocol"

// A protocol.ID routes an incoming stream to a handler, the way a URL path
// routes an HTTP request. libp2p negotiates it when the stream opens, so a peer
// that does not implement one of these fails immediately and clearly rather
// than sending bytes nobody is listening for.
//
// The trailing version is not decoration. A future "/weavefs/get/2.0.0" can be
// served alongside this one, letting nodes on different builds keep talking
// while a format change rolls out.
const (
	// StoreProtocol carries a blob from its origin to a peer that will keep a
	// copy. The stream carries a StoreRequest, then the blob, then a
	// StoreResponse comes back.
	StoreProtocol protocol.ID = "/weavefs/store/1.0.0"

	// GetProtocol asks a peer to return a blob it is holding. The stream
	// carries a GetRequest, then a GetResponse comes back, followed by the blob
	// if the peer had it.
	GetProtocol protocol.ID = "/weavefs/get/1.0.0"
)
