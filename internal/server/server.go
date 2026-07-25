// Package server composes networking with storage to make a working file
// server.
//
// internal/node knows how to reach other machines and nothing about files.
// internal/store knows how to keep files and nothing about machines. This
// package is the only place the two meet, which is what keeps either of them
// independently testable.
//
// # What a peer holds
//
// When this node asks another to keep a copy of a file, what crosses the wire
// is the blob exactly as it sits on our disk: still encrypted, under our key.
// The peer writes those bytes down without ever being able to read them. It is
// a custodian, not a reader.
//
// Two consequences are worth stating plainly, because they are the price of
// that property rather than oversights. A custodian can only ever hand the blob
// back to us — it cannot serve anyone else, since nobody else's key would
// decrypt it. And if this node is lost for good, so is the data, even though
// the custodian still holds every byte. The replica protects against our disk
// failing, not against us disappearing.
//
// The payoff is that fetching is almost trivial: the bytes that come back are
// the very bytes we encrypted, so we write them down verbatim and read them
// through the ordinary decrypting path.
package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Sambodhi-Roy/weaveFS/internal/node"
	"github.com/Sambodhi-Roy/weaveFS/internal/proto"
	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

// defaultRequestTimeout bounds a single request. It is generous because a
// request can be a multi-gigabyte file transfer over a slow link, and the thing
// it exists to prevent is not slowness but a peer that stops talking
// altogether, holding a goroutine and a file handle open forever.
const defaultRequestTimeout = 5 * time.Minute

// Config describes how a FileServer behaves. Node and Store are required;
// everything else has a working default.
type Config struct {
	// Node supplies this machine's identity and its connections to peers.
	Node *node.Node

	// Store is the local disk. Its Root should be the same directory as the
	// node's DataDir, so one node is one directory.
	Store *store.Store

	// ReplicationFactor caps how many peers receive a copy of each write.
	// Zero, the default, means every connected peer.
	//
	// Sending to everyone does not scale and ignores whether a peer has room,
	// but it is correct on the small network weaveFS currently targets. The
	// knob exists so the policy is visible rather than hidden in a loop. Real
	// placement is deferred work; see
	// improvements -for-future/06-replication-and-placement.md.
	ReplicationFactor int

	// RequestTimeout bounds one request. Defaults to defaultRequestTimeout.
	RequestTimeout time.Duration

	// Authorizer decides who may store and fetch. Defaults to AllowAll, which
	// permits everything.
	Authorizer Authorizer
}

// FileServer answers requests from peers and makes them on this node's behalf.
type FileServer struct {
	cfg Config

	// nodeID is this node's PeerID, cached because it is needed on every
	// operation and cannot change while the process runs. It is the namespace
	// every file this node owns is filed under.
	nodeID string

	startOnce sync.Once
	closeOnce sync.Once
}

// New validates cfg and returns a FileServer that is not yet listening. Call
// Start to register its protocol handlers.
func New(cfg Config) (*FileServer, error) {
	if cfg.Node == nil {
		return nil, fmt.Errorf("server: Node is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("server: Store is required")
	}
	if cfg.ReplicationFactor < 0 {
		return nil, fmt.Errorf("server: ReplicationFactor cannot be negative, got %d",
			cfg.ReplicationFactor)
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.Authorizer == nil {
		cfg.Authorizer = AllowAll{}
	}

	return &FileServer{cfg: cfg, nodeID: cfg.Node.ID()}, nil
}

// ID returns the PeerID this node files its own data under.
func (s *FileServer) ID() string { return s.nodeID }

// Start registers the protocol handlers, after which peers can store files here
// and ask for them back. It is safe to call more than once.
//
// There is no event loop and no accept goroutine to manage. libp2p invokes a
// handler in its own fresh goroutine for every incoming stream, so requests are
// served concurrently and a slow transfer cannot block anything else.
func (s *FileServer) Start() error {
	s.startOnce.Do(func() {
		h := s.cfg.Node.Host()
		h.SetStreamHandler(proto.StoreProtocol, s.handleStore)
		h.SetStreamHandler(proto.GetProtocol, s.handleGet)

		log.Printf("[server] %s serving %s and %s",
			s.nodeID, proto.StoreProtocol, proto.GetProtocol)
	})
	return nil
}

// Close stops answering requests. It deliberately does not close the Node or
// the Store: this server did not create them, and the caller may still want
// them. It is safe to call more than once.
func (s *FileServer) Close() error {
	s.closeOnce.Do(func() {
		h := s.cfg.Node.Host()
		h.RemoveStreamHandler(proto.StoreProtocol)
		h.RemoveStreamHandler(proto.GetProtocol)
	})
	return nil
}

// Store writes a file to local disk and replicates it to peers.
//
// The local write is what determines success. Replication is best effort: a
// peer that is unreachable or refuses is logged and the call still returns the
// version that was written. Failing a user's write because some third party was
// unreachable would be a worse default, and doing better needs acknowledged
// replication that weaveFS does not have yet.
func (s *FileServer) Store(ctx context.Context, key string, r io.Reader) (store.VersionEntry, error) {
	return s.StoreVersion(ctx, key, "", r)
}

// StoreVersion is Store with a message attached to the version, the way a
// commit message is attached to a commit.
func (s *FileServer) StoreVersion(ctx context.Context, key, message string, r io.Reader) (store.VersionEntry, error) {
	entry, err := s.cfg.Store.WriteVersion(s.nodeID, key, message, r)
	if err != nil {
		return store.VersionEntry{}, fmt.Errorf("server: Store %q: local write: %w", key, err)
	}

	s.replicate(ctx, key, entry)
	return entry, nil
}

// replicate sends one version to every selected peer, concurrently, and waits
// for them all to finish or fail.
func (s *FileServer) replicate(ctx context.Context, key string, entry store.VersionEntry) {
	peers := s.selectPeers()
	if len(peers) == 0 {
		log.Printf("[server] no peers connected, so %q exists only on this node", key)
		return
	}

	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if err := s.sendBlob(ctx, p, key, entry); err != nil {
				log.Printf("[server] replicating %q to %s failed: %v", key, p, err)
				return
			}
			log.Printf("[server] replicated %q to %s", key, p)
		}()
	}
	wg.Wait()
}

// selectPeers returns the peers that should receive a copy.
//
// The list comes from libp2p's live view of open connections, so a peer that
// has gone away is simply absent. There is no peer map of our own to keep in
// step, which is the bug the reference implementation never fixed.
func (s *FileServer) selectPeers() []peer.ID {
	peers := s.cfg.Node.Peers()

	if s.cfg.ReplicationFactor > 0 && len(peers) > s.cfg.ReplicationFactor {
		return peers[:s.cfg.ReplicationFactor]
	}
	return peers
}

// sendBlob pushes one version to one peer over a stream of its own.
func (s *FileServer) sendBlob(ctx context.Context, to peer.ID, key string, entry store.VersionEntry) error {
	// Read the blob raw — encrypted, exactly as stored. This happens once per
	// peer rather than being buffered in memory and shared, because a file
	// server that holds every file it writes in RAM falls over on the first
	// large one.
	blobBytes, rc, err := s.cfg.Store.ReadVersionRaw(s.nodeID, key, entry.VersionID)
	if err != nil {
		return fmt.Errorf("reading local blob: %w", err)
	}
	defer rc.Close()

	stream, err := s.cfg.Node.Host().NewStream(ctx, to, proto.StoreProtocol)
	if err != nil {
		return fmt.Errorf("opening stream: %w", err)
	}
	defer stream.Close()

	if err := stream.SetDeadline(time.Now().Add(s.cfg.RequestTimeout)); err != nil {
		return fmt.Errorf("setting deadline: %w", err)
	}

	req := proto.StoreRequest{
		OriginID: s.nodeID,
		Key:      key,
		Meta:     metaFromEntry(entry, blobBytes),
	}
	if err := proto.WriteMessage(stream, req); err != nil {
		_ = stream.Reset()
		return err
	}

	if _, err := io.Copy(stream, rc); err != nil {
		_ = stream.Reset()
		return fmt.Errorf("sending blob: %w", err)
	}

	// Half-close: we have nothing more to send, but we still want the reply.
	if err := stream.CloseWrite(); err != nil {
		_ = stream.Reset()
		return fmt.Errorf("closing the write side: %w", err)
	}

	var resp proto.StoreResponse
	if err := proto.ReadMessage(stream, &resp); err != nil {
		_ = stream.Reset()
		return fmt.Errorf("reading response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("peer refused: %s", resp.Error)
	}
	return nil
}

// Get returns a file, fetching it from a peer if this node no longer holds it.
//
// Peers are tried one at a time until one succeeds, rather than asking all of
// them at once. On a small network the worst case is milliseconds, and it
// transfers exactly one copy — the reference implementation broadcasts and then
// reads a full copy from every peer, each overwriting the last on disk.
func (s *FileServer) Get(ctx context.Context, key string) (int64, io.ReadCloser, error) {
	if s.cfg.Store.Has(s.nodeID, key) {
		return s.cfg.Store.Read(s.nodeID, key)
	}

	peers := s.cfg.Node.Peers()
	if len(peers) == 0 {
		return 0, nil, fmt.Errorf(
			"server: Get %q: not stored on this node and no peers are connected", key)
	}

	for _, p := range peers {
		if err := s.fetchBlob(ctx, p, key); err != nil {
			log.Printf("[server] %s could not supply %q: %v", p, key, err)
			continue
		}

		log.Printf("[server] recovered %q from %s", key, p)
		return s.cfg.Store.Read(s.nodeID, key)
	}

	return 0, nil, fmt.Errorf("server: Get %q: none of the %d connected peers hold it",
		key, len(peers))
}

// fetchBlob asks one peer for a blob and, if it has it, stores the returned
// bytes verbatim.
//
// Writing them raw is what makes this work. They are this node's own
// ciphertext, so once they are back on disk the ordinary decrypting read path
// can open them as if they had never left.
func (s *FileServer) fetchBlob(ctx context.Context, from peer.ID, key string) error {
	stream, err := s.cfg.Node.Host().NewStream(ctx, from, proto.GetProtocol)
	if err != nil {
		return fmt.Errorf("opening stream: %w", err)
	}
	defer stream.Close()

	if err := stream.SetDeadline(time.Now().Add(s.cfg.RequestTimeout)); err != nil {
		return fmt.Errorf("setting deadline: %w", err)
	}

	req := proto.GetRequest{OriginID: s.nodeID, Key: key}
	if err := proto.WriteMessage(stream, req); err != nil {
		_ = stream.Reset()
		return err
	}
	if err := stream.CloseWrite(); err != nil {
		_ = stream.Reset()
		return fmt.Errorf("closing the write side: %w", err)
	}

	var resp proto.GetResponse
	if err := proto.ReadMessage(stream, &resp); err != nil {
		_ = stream.Reset()
		return fmt.Errorf("reading response: %w", err)
	}

	if !resp.Found {
		// Not holding the file is an ordinary answer, not a failure. Ask
		// somebody else.
		if resp.Error != "" {
			return fmt.Errorf("peer declined: %s", resp.Error)
		}
		return fmt.Errorf("peer does not hold it")
	}
	if resp.Meta == nil {
		_ = stream.Reset()
		return fmt.Errorf("peer said it found the blob but sent no metadata")
	}
	if resp.Meta.BlobBytes < 0 {
		_ = stream.Reset()
		return fmt.Errorf("peer announced a negative blob size (%d)", resp.Meta.BlobBytes)
	}

	body := &countingReader{r: io.LimitReader(stream, resp.Meta.BlobBytes)}

	if err := s.cfg.Store.WriteVersionRaw(s.nodeID, key, entryFromMeta(*resp.Meta), body); err != nil {
		_ = stream.Reset()
		return fmt.Errorf("storing the fetched blob: %w", err)
	}

	if body.n != resp.Meta.BlobBytes {
		// A short transfer must not be mistaken for a complete file. CTR gives
		// no integrity, so a truncated blob would decrypt to truncated
		// plaintext without a word of complaint.
		_ = s.cfg.Store.DeleteVersion(s.nodeID, key, resp.Meta.VersionID)
		_ = stream.Reset()
		return fmt.Errorf("peer promised %d bytes but sent %d", resp.Meta.BlobBytes, body.n)
	}
	return nil
}

// ListVersions returns the version history this node holds for one of its own
// keys, oldest first.
func (s *FileServer) ListVersions(key string) ([]store.VersionEntry, error) {
	return s.cfg.Store.ListVersions(s.nodeID, key)
}

// metaFromEntry converts a stored version into its on-the-wire description.
//
// blobBytes is supplied separately because it is not a property of the stored
// entry: it is how many bytes this particular transfer will carry, which is the
// plaintext length plus whatever the cipher prepends.
func metaFromEntry(entry store.VersionEntry, blobBytes int64) proto.VersionMeta {
	return proto.VersionMeta{
		VersionID: entry.VersionID,
		Seq:       entry.Seq,
		CreatedAt: entry.CreatedAt,
		SizeBytes: entry.SizeBytes,
		BlobBytes: blobBytes,
		Message:   entry.Message,
	}
}

// entryFromMeta converts a peer's description back into an index entry.
//
// BlobBytes is deliberately dropped: it describes a transfer, not a file, and
// the entry's SizeBytes must stay the plaintext length even on a node that has
// never seen the plaintext.
func entryFromMeta(meta proto.VersionMeta) store.VersionEntry {
	return store.VersionEntry{
		VersionID: meta.VersionID,
		Seq:       meta.Seq,
		CreatedAt: meta.CreatedAt,
		SizeBytes: meta.SizeBytes,
		Message:   meta.Message,
	}
}

// countingReader tallies the bytes read through it, so a caller can check that
// a peer sent as many as it promised.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
