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

// ShareRequest hands a peer a readable file to keep as its own. The file's
// plaintext bytes follow this header on the same stream.
//
// This is the counterpart to StoreRequest, and the differences are the whole
// point. A StoreRequest carries an OriginID, because a custodian files the blob
// under the *sender's* namespace and keeps it sealed. A ShareRequest carries no
// such field: the recipient files the file under its *own* namespace, encrypts
// it under its *own* key, and owns it outright. There is nobody else's namespace
// to name.
//
// Like every request here, it names no sender. libp2p's Noise handshake already
// proved which peer is on the other end, and that proof is read from the
// connection (stream.Conn().RemotePeer()), never trusted from the message body.
type ShareRequest struct {
	// Key is the name the recipient will file the file under, in its own
	// namespace. If the recipient already owns a key by this name, the shared
	// file becomes a new version of it.
	Key string `json:"key"`

	// Message is the optional note the sender attached, recorded as the new
	// version's message the way a commit message is attached to a commit.
	Message string `json:"message,omitempty"`

	// SizeBytes is how many plaintext bytes follow on this stream. The recipient
	// reads exactly this many and refuses a transfer that falls short, since a
	// truncated file would otherwise decrypt to a smaller-but-valid-looking one.
	//
	// Unlike a custodian transfer there is no separate blob length: the recipient
	// re-encrypts as it stores, so the bytes on the wire and the bytes the user
	// means by "the file" are the same plaintext, and one number describes both.
	SizeBytes int64 `json:"size_bytes"`
}

// ShareResponse reports whether the recipient accepted a shared file and, if so,
// what it named the version it created.
type ShareResponse struct {
	OK bool `json:"ok"`

	// VersionID is the version the recipient created, so the sender can report
	// where its file landed. Empty on failure.
	VersionID string `json:"version_id,omitempty"`

	// Seq is the recipient's human-readable counter for that version. Display
	// only, and node-local: it is the recipient's sequence number, never to be
	// compared against the sender's. Empty (zero) on failure.
	Seq int `json:"seq,omitempty"`

	// Error explains a refusal or a failure. Empty when OK is true.
	Error string `json:"error,omitempty"`
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
