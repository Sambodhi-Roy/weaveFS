package main

// The four commands a person actually uses: put, get, ls and rm. Each one is a
// flag set, a client, one HTTP call and some printing.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runPut stores a local file on a running node.
//
//	weavefs put -data DIR <key> <file>
func runPut(args []string) error {
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")
	message := fs.String("m", "", "a message describing this version, like a commit message")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: weavefs put -data DIR <key> <file>")
	}
	key, path := fs.Arg(0), fs.Arg(1)

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer file.Close()

	c, err := newClient(*dataDir)
	if err != nil {
		return err
	}

	// The file is handed to the HTTP client as a reader, not read into memory,
	// so putting a file larger than RAM works.
	req, err := http.NewRequest(http.MethodPut, c.url("/v1/files", map[string]string{
		"key":     key,
		"message": *message,
	}), file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	// Setting ContentLength lets the node announce a size and avoids chunked
	// encoding. Stat is cheap and the file is already open.
	if info, err := file.Stat(); err == nil {
		req.ContentLength = info.Size()
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}

	var result writeResult
	if err := decodeJSON(resp, &result); err != nil {
		return err
	}

	printWriteResult(result, loadAliases(*dataDir))
	return nil
}

// writeResult mirrors the JSON the API returns from a put.
type writeResult struct {
	Key         string    `json:"key"`
	VersionID   string    `json:"version_id"`
	Seq         int       `json:"seq"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	PeersTried  int       `json:"peers_tried"`
	PeersStored int       `json:"peers_stored"`
	Failures    []struct {
		Peer  string `json:"peer"`
		Error string `json:"error"`
	} `json:"failures"`
}

// printWriteResult reports what the write achieved, separating what is certain
// from what is not.
//
// The local write succeeded or this function would not have been reached. What
// the user cannot otherwise know is whether the file reached anybody else, and
// silence there would read as success — which is exactly the lie this command
// was held back for until the file server could answer the question.
func printWriteResult(r writeResult, aliases map[string]string) {
	fmt.Printf("stored %s v%d (%s) locally\n", r.Key, r.Seq, humanBytes(r.SizeBytes))

	switch {
	case r.PeersTried == 0:
		fmt.Printf("WARNING: no peers were connected — this file exists on this node only\n")
	case r.PeersStored == 0:
		fmt.Printf("WARNING: replicated to 0 of %s — this file exists on this node only\n",
			plural(r.PeersTried, "peer"))
	default:
		fmt.Printf("replicated to %d of %s\n", r.PeersStored, plural(r.PeersTried, "peer"))
	}

	for _, f := range r.Failures {
		fmt.Printf("  %s did not take a copy: %s\n", formatPeer(f.Peer, aliases), f.Error)
	}
}

// runSend hands a readable copy of a file to one or more peers.
//
// This is the deliberate counterpart to put. put keeps a file for you and
// scatters unreadable custodian copies of it; send gives a file away, decrypted,
// to peers that will own and be able to read it. Once sent, it cannot be
// recalled — a decentralised system has no way to reach into another machine and
// take bytes back.
//
//	weavefs send -data DIR [ -peer PID | -n N | -all ] <key> [file]
//
// With a file argument the file is a loose one off disk, filed under <key>.
// Without one, <key> names a file already in this node's store.
func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")
	message := fs.String("m", "", "a note describing this version, like a commit message")
	version := fs.String("version", "", "a specific version ID to send (default: the latest)")
	as := fs.String("as", "", "the key the recipient files it under (default: the same key)")
	count := fs.Int("n", 0, "send to this many connected peers")
	all := fs.Bool("all", false, "send to every connected peer")

	var peers repeatedFlag
	fs.Var(&peers, "peer", "a specific peer PeerID to send to; may be repeated")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("usage: weavefs send -data DIR [-peer PID | -n N | -all] <key> [file]")
	}
	if err := checkOneTarget(peers, *count, *all); err != nil {
		return err
	}
	key := fs.Arg(0)

	c, err := newClient(*dataDir)
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("key", key)
	if *message != "" {
		q.Set("message", *message)
	}
	if *version != "" {
		q.Set("version", *version)
	}
	if *as != "" {
		q.Set("as", *as)
	}
	for _, p := range peers {
		q.Add("peer", p)
	}
	if *count > 0 {
		q.Set("n", strconv.Itoa(*count))
	}
	if *all {
		q.Set("all", "true")
	}

	// A file argument means a loose file: stream it as the request body. No file
	// argument shares a key already in the node's store, and sends no body.
	var body io.Reader
	var contentLength int64 = -1
	if fs.NArg() == 2 {
		path := fs.Arg(1)
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", path, err)
		}
		defer file.Close()

		body = file
		if info, err := file.Stat(); err == nil {
			contentLength = info.Size()
		}
	}

	req, err := http.NewRequest(http.MethodPost, c.urlWith("/v1/share", q), body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
		if contentLength >= 0 {
			req.ContentLength = contentLength
		}
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}

	var result shareResult
	if err := decodeJSON(resp, &result); err != nil {
		return err
	}

	printShareResult(result, loadAliases(*dataDir))
	return nil
}

// checkOneTarget requires exactly one way of naming who receives a share, so a
// forgotten target does not silently send to nobody and two targets do not
// contradict each other.
func checkOneTarget(peers repeatedFlag, count int, all bool) error {
	modes := 0
	if len(peers) > 0 {
		modes++
	}
	if count > 0 {
		modes++
	}
	if all {
		modes++
	}

	switch {
	case modes == 0:
		return fmt.Errorf("name a target: -peer <PeerID> (repeatable), -n <count>, or -all")
	case modes > 1:
		return fmt.Errorf("use only one of -peer, -n, or -all")
	}
	return nil
}

// shareResult mirrors the JSON the API returns from a share.
type shareResult struct {
	Key         string `json:"key"`
	PeersTried  int    `json:"peers_tried"`
	PeersStored int    `json:"peers_stored"`
	Placements  []struct {
		Peer      string `json:"peer"`
		VersionID string `json:"version_id"`
		Seq       int    `json:"seq"`
	} `json:"placements"`
	Failures []struct {
		Peer  string `json:"peer"`
		Error string `json:"error"`
	} `json:"failures"`
}

// printShareResult reports which peers took the file and as which version, and
// which did not and why.
func printShareResult(r shareResult, aliases map[string]string) {
	if r.PeersStored == 0 {
		fmt.Printf("shared %s with 0 of %s — nobody took it\n", r.Key, plural(r.PeersTried, "peer"))
	} else {
		fmt.Printf("shared %s with %d of %s\n", r.Key, r.PeersStored, plural(r.PeersTried, "peer"))
	}

	for _, p := range r.Placements {
		fmt.Printf("  %s stored it as v%d\n", formatPeer(p.Peer, aliases), p.Seq)
	}
	for _, f := range r.Failures {
		fmt.Printf("  %s did not take it: %s\n", formatPeer(f.Peer, aliases), f.Error)
	}
}

// runGet fetches a file from a running node, which fetches it from a peer if it
// no longer holds it.
//
//	weavefs get -data DIR <key> [outfile]
func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")
	version := fs.String("version", "", "a specific version ID (default: the latest)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("usage: weavefs get -data DIR <key> [outfile]")
	}
	key := fs.Arg(0)

	c, err := newClient(*dataDir)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, c.url("/v1/files", map[string]string{
		"key":     key,
		"version": *version,
	}), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}
	defer resp.Body.Close()

	// With no output file the bytes go to stdout, so "weavefs get ... | less"
	// works. Nothing else may be printed to stdout in that case, which is why
	// the confirmation below is conditional.
	if fs.NArg() == 1 {
		_, err := io.Copy(os.Stdout, resp.Body)
		return err
	}

	outPath := fs.Arg(1)
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", outPath, err)
	}
	defer out.Close()

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	fmt.Printf("wrote %s to %s\n", humanBytes(n), outPath)
	return nil
}

// runList prints the version history a node holds for a key.
//
//	weavefs ls -data DIR <key>
func runList(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: weavefs ls -data DIR <key>")
	}
	key := fs.Arg(0)

	c, err := newClient(*dataDir)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, c.url("/v1/versions", map[string]string{
		"key": key,
	}), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}

	var payload struct {
		Key      string `json:"key"`
		Versions []struct {
			VersionID string    `json:"version_id"`
			Seq       int       `json:"seq"`
			SizeBytes int64     `json:"size_bytes"`
			CreatedAt time.Time `json:"created_at"`
			Message   string    `json:"message"`
		} `json:"versions"`
	}
	if err := decodeJSON(resp, &payload); err != nil {
		return err
	}

	if len(payload.Versions) == 0 {
		fmt.Printf("%s has no versions on this node\n", key)
		return nil
	}

	fmt.Printf("%s — %s, oldest first\n\n", key, plural(len(payload.Versions), "version"))
	for _, v := range payload.Versions {
		fmt.Printf("  v%-3d %-10s %s  %s\n",
			v.Seq,
			humanBytes(v.SizeBytes),
			v.CreatedAt.Local().Format("2006-01-02 15:04:05"),
			v.VersionID)

		if v.Message != "" {
			fmt.Printf("       %s\n", v.Message)
		}
	}
	return nil
}

// runRemove deletes every local version of a key.
//
//	weavefs rm -data DIR <key>
func runRemove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: weavefs rm -data DIR <key>")
	}
	key := fs.Arg(0)

	c, err := newClient(*dataDir)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodDelete, c.url("/v1/files", map[string]string{
		"key": key,
	}), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}
	resp.Body.Close()

	// Saying what was *not* deleted matters more than saying what was. A user
	// who expects rm to erase every copy everywhere would otherwise be wrong
	// without being told.
	fmt.Printf("deleted every local version of %s\n", key)
	fmt.Printf("peers keep the copies they were given; a get can still recover it\n")
	return nil
}

// humanBytes renders a byte count the way a person reads one.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// plural renders a count with its noun, adding an "s" when there is not
// exactly one.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// short abbreviates a PeerID for display. They are long, and the last handful
// of characters is enough to tell two peers apart by eye.
func short(peerID string) string {
	if len(peerID) <= 12 {
		return peerID
	}
	return "…" + strings.ToLower(peerID[len(peerID)-8:])
}

// formatPeer returns the alias for a peer if one is set, or the short ID.
func formatPeer(peerID string, aliases map[string]string) string {
	if name, ok := aliases[peerID]; ok {
		return name
	}
	return short(peerID)
}

func loadAliases(dataDir string) map[string]string {
	aliases := make(map[string]string)
	path := filepath.Join(dataDir, "aliases.json")
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &aliases)
	}
	return aliases
}

func saveAliases(dataDir string, aliases map[string]string) error {
	path := filepath.Join(dataDir, "aliases.json")
	data, err := json.MarshalIndent(aliases, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// runAlias assigns a readable name to a Peer ID.
//
//	weavefs alias -data DIR <peer_id> <name>
func runAlias(args []string) error {
	fs := flag.NewFlagSet("alias", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: weavefs alias -data DIR <peer_id> <name>")
	}
	peerID, name := fs.Arg(0), fs.Arg(1)

	aliases := loadAliases(*dataDir)
	aliases[peerID] = name
	if err := saveAliases(*dataDir, aliases); err != nil {
		return fmt.Errorf("saving aliases: %w", err)
	}

	fmt.Printf("alias set: %s -> %s\n", short(peerID), name)
	return nil
}

// runAliases lists all saved aliases.
//
//	weavefs aliases -data DIR
func runAliases(args []string) error {
	fs := flag.NewFlagSet("aliases", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: weavefs aliases -data DIR")
	}

	aliases := loadAliases(*dataDir)
	if len(aliases) == 0 {
		fmt.Println("no aliases saved")
		return nil
	}

	fmt.Println("Aliases:")
	for peerID, name := range aliases {
		fmt.Printf("  %-15s %s\n", name, peerID)
	}
	return nil
}

// runDisconnect explicitly closes a connection to a peer.
//
//	weavefs disconnect -data DIR <peer_id>
func runDisconnect(args []string) error {
	fs := flag.NewFlagSet("disconnect", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "node directory (or set WEAVEFS_DATA)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: weavefs disconnect -data DIR <peer_id>")
	}
	peerID := fs.Arg(0)

	c, err := newClient(*dataDir)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodDelete, c.url("/v1/peers", map[string]string{
		"peer": peerID,
	}), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(resp)
	}
	resp.Body.Close()

	aliases := loadAliases(*dataDir)
	fmt.Printf("disconnected from %s\n", formatPeer(peerID, aliases))
	return nil
}
