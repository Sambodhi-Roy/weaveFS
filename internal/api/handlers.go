package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Sambodhi-Roy/weaveFS/internal/server"
	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

// routes maps HTTP methods and paths to handlers.
//
// Go 1.22 and later let a ServeMux pattern carry the method, so "PUT /v1/files"
// and "GET /v1/files" are two separate registrations and anything else on that
// path gets a 405 from the mux without a line of our code.
//
// The key travels as a query parameter rather than a path segment. Keys are
// arbitrary strings that may contain slashes, and "/v1/files?key=notes/2026/q1"
// needs no escaping rules at all, whereas "/v1/files/notes/2026/q1" needs a
// convention for where the key starts and how a literal slash inside one is
// spelled. The version prefix exists so a later, incompatible shape can be
// added at /v2 without breaking a client.
func (a *API) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /v1/files", a.handlePut)
	mux.HandleFunc("GET /v1/files", a.handleGet)
	mux.HandleFunc("DELETE /v1/files", a.handleDelete)
	mux.HandleFunc("POST /v1/share", a.handleShare)
	mux.HandleFunc("GET /v1/versions", a.handleVersions)
	mux.HandleFunc("GET /v1/id", a.handleID)

	return logRequests(mux)
}

// writeResultJSON is the JSON shape of a server.WriteResult.
//
// It exists because WriteResult carries Go error values in its Failures, and an
// error has no useful JSON form — encoding/json would render it as an empty
// object. Converting here also keeps internal/server free of JSON tags: the
// wire format of the client API is this package's business, not the file
// server's.
type writeResultJSON struct {
	Key         string        `json:"key"`
	VersionID   string        `json:"version_id"`
	Seq         int           `json:"seq"`
	SizeBytes   int64         `json:"size_bytes"`
	CreatedAt   time.Time     `json:"created_at"`
	Message     string        `json:"message,omitempty"`
	PeersTried  int           `json:"peers_tried"`
	PeersStored int           `json:"peers_stored"`
	Failures    []failureJSON `json:"failures,omitempty"`
}

// failureJSON names one peer that did not take a copy, with its reason
// flattened to a string.
type failureJSON struct {
	Peer  string `json:"peer"`
	Error string `json:"error"`
}

// versionJSON describes one stored version to a client.
type versionJSON struct {
	VersionID string    `json:"version_id"`
	Seq       int       `json:"seq"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	Message   string    `json:"message,omitempty"`
}

// handlePut stores the request body under the given key.
//
// The body is streamed straight into the file server rather than read into
// memory first, so storing a file larger than RAM works.
func (a *API) handlePut(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}
	message := r.URL.Query().Get("message")

	result, err := a.cfg.FileServer.StoreVersion(r.Context(), key, message, r.Body)
	if err != nil {
		// The local write failed, which is the only thing that makes a store
		// unsuccessful. Replication failures live in the result, not here.
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, toWriteResultJSON(key, result))
}

// handleGet returns the file bytes for a key, fetching from a peer if this node
// no longer holds it.
func (a *API) handleGet(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}
	version := r.URL.Query().Get("version")

	size, rc, err := a.cfg.FileServer.GetVersion(r.Context(), key, version)
	if err != nil {
		// A key that is not here and not on any peer is a 404 rather than a
		// 500: nothing malfunctioned, the file simply is not available.
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer rc.Close()

	// Announcing the length lets a client show progress and detect truncation.
	// It is the plaintext length: the store reports plaintext sizes even though
	// the bytes on disk are longer by the cipher's header.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)

	// A failure here cannot become an error response — the status line and
	// headers are already sent — so it is logged and the connection is left to
	// close short, which the Content-Length lets the client notice.
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("[api] sending %q failed partway: %v", key, err)
	}
}

// shareResultJSON is the JSON shape of a server.ShareResult.
type shareResultJSON struct {
	Key         string          `json:"key"`
	PeersTried  int             `json:"peers_tried"`
	PeersStored int             `json:"peers_stored"`
	Placements  []placementJSON `json:"placements,omitempty"`
	Failures    []failureJSON   `json:"failures,omitempty"`
}

// placementJSON records where one peer filed a shared file.
type placementJSON struct {
	Peer      string `json:"peer"`
	VersionID string `json:"version_id"`
	Seq       int    `json:"seq"`
}

// handleShare sends a readable copy of a file to one or more peers.
//
// It carries two shapes on one endpoint, told apart by whether the request has a
// body. With a body, the body is a loose file: it is stored locally under the
// key first (without custodian replication) and then shared. Without a body, an
// existing stored key is shared as-is.
func (a *API) handleShare(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	message := q.Get("message")
	version := q.Get("version")
	destKey := q.Get("as")

	targets, err := a.parseTargets(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// A body means a loose file that is not yet in the store. Bring it in first,
	// locally only, so the share below can re-read it per peer.
	if r.ContentLength != 0 {
		if _, err := a.cfg.FileServer.StoreLocal(key, message, r.Body); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	result, err := a.cfg.FileServer.Share(r.Context(), targets, key, version, destKey, message)
	if err != nil {
		// The source key is not on this node, or its version could not be read.
		writeError(w, http.StatusNotFound, err)
		return
	}

	writeJSON(w, http.StatusOK, toShareResultJSON(key, result))
}

// parseTargets reads the targeting choice from the query string and resolves it
// to concrete peers. Exactly one of peer / n / all is expected.
func (a *API) parseTargets(r *http.Request) ([]peer.ID, error) {
	q := r.URL.Query()

	var explicit []peer.ID
	for _, ps := range q["peer"] {
		pid, err := peer.Decode(ps)
		if err != nil {
			return nil, fmt.Errorf("invalid peer id %q: %w", ps, err)
		}
		explicit = append(explicit, pid)
	}

	count := 0
	if s := q.Get("n"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("n must be a positive integer, got %q", s)
		}
		count = n
	}

	all := q.Get("all") == "true"

	return a.cfg.FileServer.ResolveTargets(explicit, count, all)
}

// handleDelete removes every local version of a key.
func (a *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}

	if err := a.cfg.FileServer.Delete(key); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"key": key, "deleted": true})
}

// handleVersions lists the version history this node holds for a key.
func (a *API) handleVersions(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}

	versions, err := a.cfg.FileServer.ListVersions(key)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	// An empty slice rather than nil, so the JSON is [] and not null.
	out := make([]versionJSON, 0, len(versions))
	for _, v := range versions {
		out = append(out, toVersionJSON(v))
	}

	writeJSON(w, http.StatusOK, map[string]any{"key": key, "versions": out})
}

// handleID reports which node is answering, so a client can confirm it reached
// the process it meant to.
func (a *API) handleID(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"peer_id": a.cfg.FileServer.ID(),
	})
}

// requireKey pulls the mandatory "key" query parameter, answering the request
// itself if it is missing. The bool reports whether the caller should continue.
func requireKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the 'key' query parameter is required"))
		return "", false
	}
	return key, true
}

// writeJSON sends one JSON value with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] could not encode a response: %v", err)
	}
}

// writeError sends an error as JSON, so a client parses failures the same way
// it parses successes.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// toWriteResultJSON flattens a WriteResult for the wire.
func toWriteResultJSON(key string, r server.WriteResult) writeResultJSON {
	out := writeResultJSON{
		Key:         key,
		VersionID:   r.Entry.VersionID,
		Seq:         r.Entry.Seq,
		SizeBytes:   r.Entry.SizeBytes,
		CreatedAt:   r.Entry.CreatedAt,
		Message:     r.Entry.Message,
		PeersTried:  r.PeersTried,
		PeersStored: r.PeersStored,
	}

	for _, f := range r.Failures {
		out.Failures = append(out.Failures, failureJSON{
			Peer:  f.Peer.String(),
			Error: f.Err.Error(),
		})
	}
	return out
}

// toShareResultJSON flattens a ShareResult for the wire.
func toShareResultJSON(key string, r server.ShareResult) shareResultJSON {
	out := shareResultJSON{
		Key:         key,
		PeersTried:  r.PeersTried,
		PeersStored: r.PeersStored,
	}

	for _, p := range r.Placements {
		out.Placements = append(out.Placements, placementJSON{
			Peer:      p.Peer.String(),
			VersionID: p.VersionID,
			Seq:       p.Seq,
		})
	}
	for _, f := range r.Failures {
		out.Failures = append(out.Failures, failureJSON{
			Peer:  f.Peer.String(),
			Error: f.Err.Error(),
		})
	}
	return out
}

// toVersionJSON converts one stored entry for the wire.
func toVersionJSON(v store.VersionEntry) versionJSON {
	return versionJSON{
		VersionID: v.VersionID,
		Seq:       v.Seq,
		SizeBytes: v.SizeBytes,
		CreatedAt: v.CreatedAt,
		Message:   v.Message,
	}
}

// logRequests wraps a handler so every call appears in the node's log with the
// same [api] prefix the rest of the package uses.
//
// This is the decorator pattern: the returned handler has the same type as the
// one passed in, does a little extra, and delegates. http.HandlerFunc converts
// a plain function into a http.Handler.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)

		log.Printf("[api] %s %s%s (%s)",
			r.Method, r.URL.Path, querySuffix(r), time.Since(start).Round(time.Millisecond))
	})
}

// querySuffix renders the query string for a log line, or nothing if there is
// none.
func querySuffix(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}
