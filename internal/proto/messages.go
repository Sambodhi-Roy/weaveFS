package proto

import "time"

// VersionMeta describes one stored version as it travels between nodes.
//
// It mirrors store.VersionEntry rather than reusing it. The two are separate on
// purpose: renaming a field for local storage reasons should not silently
// change the wire format and break every peer running an older build. The extra
// field here, BlobBytes, is meaningless on disk and meaningful only in flight.
//
// The strings in backticks are struct tags — metadata read at runtime by
// encoding/json to decide each field's name on the wire. "omitempty" leaves a
// field out entirely when it holds its zero value.
type VersionMeta struct {
	// VersionID is the version's identity, and must be identical on every node
	// holding this version. Without that agreement an origin could never ask a
	// custodian for one specific version.
	VersionID string `json:"version_id"`

	// Seq is the origin's human-readable counter. Display only — it is
	// node-local and must never be compared across nodes.
	Seq int `json:"seq"`

	// CreatedAt is when the origin wrote this version, in UTC.
	CreatedAt time.Time `json:"created_at"`

	// SizeBytes is the plaintext length, which is what a user means by "how big
	// is this file". It is not the number of bytes on the wire.
	SizeBytes int64 `json:"size_bytes"`

	// BlobBytes is how many bytes actually follow on this stream: the plaintext
	// plus whatever the sender's cipher prepends. Keeping this separate from
	// SizeBytes is why weaveFS avoids the reference implementation's habit of
	// hardcoding "size + 16" at the call site.
	BlobBytes int64 `json:"blob_bytes"`

	// Message is the optional note the origin attached, like a commit message.
	Message string `json:"message,omitempty"`
}

// StoreRequest asks a peer to keep a copy of a blob. The blob's bytes follow
// this header on the same stream.
type StoreRequest struct {
	// OriginID is the PeerID whose namespace this blob belongs to, and the
	// directory the custodian will file it under.
	//
	// This is a routing fact, not an identity claim, which is why it belongs in
	// the message. Who is *sending* the request is a different question, and is
	// answered by libp2p's handshake rather than by anything written here — see
	// the note on GetRequest.
	OriginID string `json:"origin_id"`

	// Key is the logical file name, e.g. "quarterly_report".
	Key string `json:"key"`

	// Meta describes the version being sent, and is recorded verbatim by the
	// custodian so both nodes agree on what this version is called.
	Meta VersionMeta `json:"meta"`
}

// StoreResponse reports whether the custodian accepted the blob.
type StoreResponse struct {
	OK bool `json:"ok"`

	// Error explains a refusal or a failure. Empty when OK is true.
	Error string `json:"error,omitempty"`
}

// GetRequest asks a peer to return a blob it is holding.
//
// Note what is absent: there is no field naming the sender. libp2p's Noise
// handshake already proved which peer is on the other end of this stream, and
// that proof is available to the handler as stream.Conn().RemotePeer(). A
// SenderID field would be a mere claim sitting next to that proof, and sooner
// or later something would trust the wrong one.
type GetRequest struct {
	// OriginID is the namespace to look in — normally the requester's own ID,
	// since a custodian holds blobs encrypted with their origin's key and can
	// serve them back to nobody else.
	OriginID string `json:"origin_id"`

	// Key is the logical file name being asked for.
	Key string `json:"key"`

	// VersionID selects a specific version. Empty means the latest.
	VersionID string `json:"version_id,omitempty"`
}

// GetResponse answers a GetRequest. When Found is true the blob's bytes follow
// this header on the same stream.
type GetResponse struct {
	// Found reports whether the peer holds the requested blob. A peer that does
	// not is answering normally, not failing — the requester simply asks
	// somebody else.
	Found bool `json:"found"`

	// Error explains a refusal or a failure. Empty on success and on an
	// ordinary "I don't have it".
	Error string `json:"error,omitempty"`

	// Meta describes the blob that follows. A pointer rather than a value so
	// that "no metadata" is representable, and omitted from the JSON entirely
	// when the answer is Found: false.
	Meta *VersionMeta `json:"meta,omitempty"`
}
