package main

import (
	"bytes"
	"fmt"
	"io"
	"log"

	"github.com/Sambodhi-Roy/weaveFS/internal/store"
)

func main() {
	s := store.NewStore(store.StoreOpts{
		Root:              "weavefs_data",
		PathTransformFunc: store.CASPathTransformFunc,
	})

	const nodeID = "local-node"
	const key = "my_document"
	data := []byte("weaveFS: distributed storage, one node at a time")

	// Write to the CAS store.
	n, err := s.Write(nodeID, key, bytes.NewReader(data))
	if err != nil {
		log.Fatalf("write error: %v", err)
	}
	fmt.Printf("wrote %d bytes for key %q\n", n, key)

	// Read back from the CAS store.
	size, rc, err := s.Read(nodeID, key)
	if err != nil {
		log.Fatalf("read error: %v", err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	fmt.Printf("read  %d bytes: %q\n", size, string(got))
}
