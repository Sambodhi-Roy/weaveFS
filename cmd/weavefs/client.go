package main

// This file is the client half of the CLI. The commands in it — put, get, ls
// and rm — do not start a node. They find one that is already running and ask
// it to do the work, which is the whole point: the node that answers has the
// real identity, the real encryption key and the real peer connections.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/Sambodhi-Roy/weaveFS/internal/api"
)

// clientTimeout bounds one client request. It is generous because a request can
// be a large file transfer, and because a get that misses locally has to wait
// on a round trip to a peer before any bytes come back.
const clientTimeout = 10 * time.Minute

// client talks to the API of a node running in another process.
type client struct {
	base string // e.g. "http://127.0.0.1:51823"
	dir  string // the node directory, kept for error messages
	http *http.Client
}

// newClient locates the node running in dataDir by reading the address file its
// API wrote on startup.
//
// A missing address file is the ordinary case of "you have not started a node",
// so it gets a message that says what to do rather than a path and an errno.
func newClient(dataDir string) (*client, error) {
	addr, err := api.ReadAddrFile(dataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, notRunningError(dataDir)
		}
		return nil, err
	}

	return &client{
		base: "http://" + addr,
		dir:  dataDir,
		http: &http.Client{Timeout: clientTimeout},
	}, nil
}

// notRunningError is the message a user gets when nothing is listening. It
// names the command that fixes it, because the whole reason this API exists is
// that a client cannot do the work by itself.
func notRunningError(dataDir string) error {
	return fmt.Errorf("no node is running in %q\n\nstart one first, in another terminal:\n  weavefs serve -data %s",
		dataDir, dataDir)
}

// do sends a request and returns the response, translating a failure to reach
// the node into the same friendly message as a missing address file.
//
// A stale address file — left behind by a node that was killed outright rather
// than interrupted, so its cleanup never ran — looks exactly like this, and it
// is the one failure a user is most likely to hit twice.
func (c *client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if isDialFailure(err) {
			return nil, notRunningError(c.dir)
		}
		return nil, fmt.Errorf("talking to the node in %q: %w", c.dir, err)
	}
	return resp, nil
}

// isDialFailure reports whether err means the connection was never established.
//
// The obvious test — errors.Is(err, syscall.ECONNREFUSED) — is quietly wrong on
// Windows. There, syscall.ECONNREFUSED is an invented placeholder value that
// package syscall defines only so portable code compiles; the error a real
// refused connection carries is WSAECONNREFUSED, a different number entirely,
// so the comparison never matches and the user gets a raw "connectex:" message.
//
// Matching on the operation instead avoids the whole platform question. The
// only address this client ever dials is the loopback one the node itself
// recorded, so a dial that does not complete means the node is not there —
// whatever the local error numbering happens to be.
func isDialFailure(err error) bool {
	var opErr *net.OpError

	// errors.As walks the chain of wrapped errors looking for one of that
	// concrete type, and fills in the pointer when it finds it. Here the chain
	// is url.Error wrapping net.OpError wrapping the operating system's error.
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// url builds an endpoint URL with query parameters properly escaped, which is
// what lets a key contain slashes, spaces or anything else.
func (c *client) url(path string, params map[string]string) string {
	q := make(url.Values, len(params))
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}

	if len(q) == 0 {
		return c.base + path
	}
	return c.base + path + "?" + q.Encode()
}

// errorFromResponse turns a non-2xx reply into a Go error, preferring the
// server's own JSON message over a bare status code.
func errorFromResponse(resp *http.Response) error {
	defer resp.Body.Close()

	var payload struct {
		Error string `json:"error"`
	}

	// The body is capped: an error message is short, and a client should not be
	// talked into reading a gigabyte because a server misbehaved.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return errors.New(payload.Error)
	}

	return fmt.Errorf("the node answered %s", resp.Status)
}

// decodeJSON reads a JSON response body into v.
func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("could not read the node's reply: %w", err)
	}
	return nil
}
