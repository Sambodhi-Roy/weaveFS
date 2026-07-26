package main

// The four commands a person actually uses: put, get, ls and rm. Each one is a
// flag set, a client, one HTTP call and some printing.

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// runPut stores a local file on a running node.
//
//	weavefs put -data DIR <key> <file>
func runPut(args []string) error {
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	dataDir := fs.String("data", "weavefs_data", "node directory")
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

	printWriteResult(result)
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
func printWriteResult(r writeResult) {
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
		fmt.Printf("  %s did not take a copy: %s\n", short(f.Peer), f.Error)
	}
}

// runGet fetches a file from a running node, which fetches it from a peer if it
// no longer holds it.
//
//	weavefs get -data DIR <key> [outfile]
func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	dataDir := fs.String("data", "weavefs_data", "node directory")
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
	dataDir := fs.String("data", "weavefs_data", "node directory")

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
	dataDir := fs.String("data", "weavefs_data", "node directory")

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
