package server

import (
	"io"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/network"

	"github.com/Sambodhi-Roy/weaveFS/internal/proto"
)

// This file is the answering side: what this node does when a peer opens a
// stream to it. libp2p calls these in a goroutine of their own, one per
// incoming stream, so they run concurrently with each other and with anything
// this node is doing on its own behalf. internal/store locks per key, which is
// what makes that safe.
//
// Three rules are followed by both handlers, and each exists because of a
// specific way the deleted transport used to fail:
//
//   - defer stream.Close(), on the first line. A handler that returns early
//     without releasing the stream used to wedge the whole connection.
//   - stream.Reset() on any error, before returning. Reset tells the peer the
//     request failed. Merely closing would give it a clean end-of-file, which
//     it would read as a complete — but truncated — response.
//   - a deadline, so a peer that stops talking mid-transfer cannot hold a
//     goroutine and a file handle open indefinitely.

// handleStore accepts a blob from a peer and files it under that peer's
// namespace, without decrypting it or being able to.
func (s *FileServer) handleStore(stream network.Stream) {
	defer stream.Close()

	// The identity of the caller comes from the connection, never from the
	// message. libp2p's Noise handshake proved which key is on the other end;
	// a PeerID written into the request body would be a mere claim.
	from := stream.Conn().RemotePeer()

	if err := stream.SetDeadline(time.Now().Add(s.cfg.RequestTimeout)); err != nil {
		log.Printf("[server] could not set a deadline for %s: %v", from, err)
		_ = stream.Reset()
		return
	}

	var req proto.StoreRequest
	if err := proto.ReadMessage(stream, &req); err != nil {
		log.Printf("[server] unreadable store request from %s: %v", from, err)
		_ = stream.Reset()
		return
	}

	if err := s.cfg.Authorizer.AllowStore(from, req.OriginID, req.Key); err != nil {
		log.Printf("[server] refused %s permission to store %q: %v", from, req.Key, err)
		s.reply(stream, proto.StoreResponse{Error: "not authorised to store here"})
		return
	}

	if req.Meta.BlobBytes < 0 {
		log.Printf("[server] %s announced a negative blob size (%d)", from, req.Meta.BlobBytes)
		s.reply(stream, proto.StoreResponse{Error: "invalid blob size"})
		return
	}

	// LimitReader stops the copy at exactly the promised length, so a peer
	// cannot keep streaming and fill this node's disk. The counter then checks
	// the other direction — that it sent everything it promised.
	body := &countingReader{r: io.LimitReader(stream, req.Meta.BlobBytes)}

	if err := s.cfg.Store.WriteVersionRaw(req.OriginID, req.Key, entryFromMeta(req.Meta), body); err != nil {
		log.Printf("[server] storing %q for %s failed: %v", req.Key, req.OriginID, err)
		s.reply(stream, proto.StoreResponse{Error: "could not store the blob"})
		return
	}

	if body.n != req.Meta.BlobBytes {
		// A truncated blob must not be recorded as a good replica. CTR provides
		// no integrity, so a short blob would later decrypt to short plaintext
		// with no error raised anywhere.
		log.Printf("[server] %s promised %d bytes of %q but sent %d; discarding",
			from, req.Meta.BlobBytes, req.Key, body.n)

		_ = s.cfg.Store.DeleteVersion(req.OriginID, req.Key, req.Meta.VersionID)
		s.reply(stream, proto.StoreResponse{Error: "incomplete blob"})
		return
	}

	log.Printf("[server] stored %d bytes of %q on behalf of %s",
		body.n, req.Key, req.OriginID)

	s.reply(stream, proto.StoreResponse{OK: true})
}

// handleGet returns a blob this node is holding, exactly as it was received.
//
// Not having the requested file is a normal answer rather than an error: the
// requester simply asks the next peer. Only a malformed request or a genuine
// failure resets the stream.
func (s *FileServer) handleGet(stream network.Stream) {
	defer stream.Close()

	from := stream.Conn().RemotePeer()

	if err := stream.SetDeadline(time.Now().Add(s.cfg.RequestTimeout)); err != nil {
		log.Printf("[server] could not set a deadline for %s: %v", from, err)
		_ = stream.Reset()
		return
	}

	var req proto.GetRequest
	if err := proto.ReadMessage(stream, &req); err != nil {
		log.Printf("[server] unreadable get request from %s: %v", from, err)
		_ = stream.Reset()
		return
	}

	if err := s.cfg.Authorizer.AllowGet(from, req.OriginID, req.Key); err != nil {
		log.Printf("[server] refused %s permission to fetch %q: %v", from, req.Key, err)
		s.reply(stream, proto.GetResponse{Error: "not authorised to read that"})
		return
	}

	// Resolve the metadata before opening the blob, so that "I don't have it"
	// is answered without a file handle ever being created.
	entry, err := s.cfg.Store.VersionInfo(req.OriginID, req.Key, req.VersionID)
	if err != nil {
		s.reply(stream, proto.GetResponse{Found: false})
		return
	}

	blobBytes, rc, err := s.cfg.Store.ReadVersionRaw(req.OriginID, req.Key, req.VersionID)
	if err != nil {
		log.Printf("[server] index lists %q for %s but the blob is unreadable: %v",
			req.Key, req.OriginID, err)
		s.reply(stream, proto.GetResponse{Found: false})
		return
	}
	defer rc.Close()

	meta := metaFromEntry(entry, blobBytes)
	if err := proto.WriteMessage(stream, proto.GetResponse{Found: true, Meta: &meta}); err != nil {
		log.Printf("[server] could not answer %s: %v", from, err)
		_ = stream.Reset()
		return
	}

	// The blob goes out untouched: still encrypted under its origin's key,
	// which this node does not have and does not need.
	n, err := io.Copy(stream, rc)
	if err != nil {
		log.Printf("[server] sending %q to %s failed after %d bytes: %v", req.Key, from, n, err)
		_ = stream.Reset()
		return
	}

	if err := stream.CloseWrite(); err != nil {
		log.Printf("[server] could not close the write side for %s: %v", from, err)
		_ = stream.Reset()
		return
	}

	log.Printf("[server] served %d bytes of %q to %s", n, req.Key, from)
}

// reply sends a final response and resets the stream if it cannot be delivered.
//
// It takes any message type because both handlers do the same thing with their
// different response structs, and the only interesting case is the failure one.
func (s *FileServer) reply(stream network.Stream, msg any) {
	if err := proto.WriteMessage(stream, msg); err != nil {
		log.Printf("[server] could not send a response to %s: %v",
			stream.Conn().RemotePeer(), err)
		_ = stream.Reset()
	}
}
