// Package api gives a running weaveFS node a door that a command-line client
// can knock on.
//
// # The problem it solves
//
// A node started with "weavefs serve" holds a libp2p host, an encryption key
// and a blob tree, and it stays running. Everything it can do lives on an
// in-process Go object. Another process — "weavefs put", say — cannot reach
// that object. Before this package the only ways to store a file were to write
// Go code against internal/server or to run the built-in demo.
//
// The tempting shortcut is to let "put" start its own node against the same
// data directory, write the file and exit. That would be two processes sharing
// one identity.key and one blob tree, with no peer connections because
// discovery takes longer than the process lives. It would print "wrote 47
// bytes" and have replicated to nobody.
//
// So instead the running node opens a small HTTP server and the client talks to
// it. The node that answers is the real one, with its real peers.
//
// # Why loopback only
//
// The listener binds to 127.0.0.1, which the operating system will not route
// off the machine. That is deliberate, and it is the reason this package needs
// no authentication of its own: weaveFS has no access-control policy yet — the
// default Authorizer permits everything (see
// improvements -for-future/02-connection-gating.md) — so an API reachable from
// the network would be an unauthenticated remote handle on somebody's disk.
// Loopback sidesteps that question honestly rather than pretending to answer
// it. Remote administration can come later, with a policy behind it.
//
// # How the client finds the port
//
// The listener asks for port 0, meaning "any free port the OS likes", so
// nothing collides when several nodes run on one machine. The address it
// actually got is then written to <DataDir>/api.addr, beside identity.key and
// encryption.key. A client is pointed at a directory, reads that file and
// dials. The file is removed on shutdown so a later client does not chase a
// dead port.
package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/Sambodhi-Roy/weaveFS/internal/server"
)

// DefaultAddr is the address the listener binds to when Config.Addr is empty.
// Port 0 lets the operating system pick a free one.
const DefaultAddr = "127.0.0.1:0"

// shutdownTimeout bounds how long Close waits for in-flight requests to finish
// before dropping them. A request can be a large file transfer, but a client
// that has stopped reading must not keep the node alive forever.
const shutdownTimeout = 10 * time.Second

// Config describes an API server. FileServer and DataDir are required.
type Config struct {
	// FileServer is the running node this API exposes. Every handler is a thin
	// wrapper over one of its methods; no storage logic lives in this package.
	FileServer *server.FileServer

	// DataDir is the node's directory. The chosen listen address is written
	// there as api.addr so clients can find it.
	DataDir string

	// Addr is the address to bind. Defaults to DefaultAddr.
	//
	// Binding to anything other than a loopback address exposes an
	// unauthenticated handle on this node's storage to the network. The field
	// exists because a fixed loopback port is sometimes convenient, not as an
	// invitation to bind 0.0.0.0.
	Addr string
}

// API is an HTTP server sitting in front of a FileServer.
type API struct {
	cfg      Config
	http     *http.Server
	listener net.Listener
}

// New validates cfg and returns an API that is not yet listening. Call Start.
func New(cfg Config) (*API, error) {
	if cfg.FileServer == nil {
		return nil, fmt.Errorf("api: FileServer is required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("api: DataDir is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}

	a := &API{cfg: cfg}
	a.http = &http.Server{Handler: a.routes()}
	return a, nil
}

// Start binds the listener and serves in the background.
//
// Binding happens synchronously so that a port already in use is reported to
// the caller as an error, rather than surfacing later on a goroutine nobody is
// watching. Only the accept loop runs in the background.
func (a *API) Start() error {
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return fmt.Errorf("api: listening on %s: %w", a.cfg.Addr, err)
	}
	a.listener = ln

	if err := writeAddrFile(a.cfg.DataDir, ln.Addr().String()); err != nil {
		_ = ln.Close()
		return err
	}

	// A goroutine is Go's very cheap thread. Serve blocks until the server is
	// shut down, so it runs on one of its own and the caller carries on.
	go func() {
		// ErrServerClosed is what Serve returns after a clean Shutdown, so it
		// is the success case rather than a failure worth logging.
		if err := a.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[api] stopped serving: %v", err)
		}
	}()

	log.Printf("[api] listening on http://%s", ln.Addr())
	return nil
}

// Addr returns the address the API is listening on, including the port the
// operating system chose. It is empty before Start.
func (a *API) Addr() string {
	if a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

// Close stops the API and removes the address file.
//
// It deliberately does not close the FileServer, the node or the store: this
// package did not create them and the caller may still want them. That mirrors
// server.FileServer.Close.
func (a *API) Close() error {
	// Remove the address file first. If shutdown is slow, a client that arrives
	// in the meantime should be told nothing is running rather than be left
	// waiting on a listener that is going away.
	removeAddrFile(a.cfg.DataDir)

	if a.http == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := a.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("api: shutting down: %w", err)
	}
	return nil
}
