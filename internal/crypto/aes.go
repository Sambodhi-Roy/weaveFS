// Package crypto provides encryption at rest for weaveFS blobs.
//
// Encryption in transit is not this package's concern: libp2p runs a Noise
// handshake on every connection, so nothing crosses the network in plaintext
// already. What this package protects against is offline access to the disk —
// a stolen laptop, a discarded drive, a leaked backup, or a peer that stores
// our blobs but must not be able to read them.
//
// The scope is worth being precise about: it does not protect against an
// attacker who is already running as this user on a live machine, since they
// can simply read the key file.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

// The on-disk layout of an encrypted blob:
//
//	┌────────────┬───────────┬──────────────┬─────────────────────┐
//	│ "WFS1"     │ 0x01      │ IV           │ ciphertext          │
//	│ 4 bytes    │ 1 byte    │ 16 bytes     │ len(plaintext)      │
//	└────────────┴───────────┴──────────────┴─────────────────────┘
//
// The magic and version prefix exist because CTR decryption cannot fail. Fed a
// plaintext blob — one written before encryption was enabled — it would XOR a
// keystream against readable text and return noise with a nil error, handing
// silent corruption to the caller as though it were success. Five bytes turn
// that into a clear error, and leave room to change the format later without
// having to guess at old files.
const (
	magic              = "WFS1"
	formatVersion byte = 0x01

	// ivSize matches the AES block size: CTR seeds its counter with one block.
	ivSize = aes.BlockSize

	// headerSize is the total per-blob overhead, in bytes.
	headerSize = len(magic) + 1 + ivSize
)

// AESCipher encrypts and decrypts blob content using AES-256 in counter (CTR)
// mode.
//
// CTR turns a block cipher into a stream cipher: it encrypts a counter to
// produce a keystream and XORs that against the data. Because XOR is its own
// inverse, encryption and decryption are the same operation, the ciphertext is
// exactly as long as the plaintext, and — the property that matters most here —
// content can be processed incrementally rather than a whole file at a time.
//
// CTR provides confidentiality but NOT integrity. A single flipped bit on disk
// flips exactly one bit of the recovered plaintext, with no error raised, and
// an attacker who knows the plaintext can make targeted edits without the key.
// Detecting that needs an authenticated mode such as AES-GCM, which cannot be
// streamed as one message and so requires a chunked blob format. See
// segment-b-encryption-at-rest.md, decision 5.
//
// An AESCipher is safe for concurrent use: cipher.Block is stateless, and every
// call to EncryptWriter or DecryptReader builds its own independent CTR stream.
type AESCipher struct {
	block cipher.Block
}

// NewAESCipher returns a cipher that uses key, which must be exactly KeySize
// bytes. Use LoadOrCreateKey to obtain one.
func NewAESCipher(key []byte) (*AESCipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeySize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: creating AES cipher: %w", err)
	}
	return &AESCipher{block: block}, nil
}

// Overhead reports how many bytes larger an encrypted blob is than its
// plaintext — the header, since CTR itself adds nothing.
func (c *AESCipher) Overhead() int64 {
	return int64(headerSize)
}

// EncryptWriter writes the blob header to w and returns a writer that encrypts
// everything subsequently written to it.
//
// The returned writer is a decorator: it satisfies io.Writer just like w does,
// so callers pass it to io.Copy without knowing encryption is happening. It is
// deliberately not an io.WriteCloser — CTR has no trailer to flush, so there is
// nothing to close, and the caller still owns w.
//
// A fresh IV is generated per call. This is not optional: encrypting two
// different plaintexts under the same key and the same IV lets anyone XOR the
// two ciphertexts together to recover the XOR of the plaintexts, with the key
// cancelling out entirely.
func (c *AESCipher) EncryptWriter(w io.Writer) (io.Writer, error) {
	iv := make([]byte, ivSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, fmt.Errorf("crypto: generating IV: %w", err)
	}

	header := make([]byte, 0, headerSize)
	header = append(header, magic...)
	header = append(header, formatVersion)
	header = append(header, iv...)

	if _, err := w.Write(header); err != nil {
		return nil, fmt.Errorf("crypto: writing blob header: %w", err)
	}

	return &cipher.StreamWriter{S: cipher.NewCTR(c.block, iv), W: w}, nil
}

// DecryptReader consumes and validates the blob header from r, then returns a
// reader yielding plaintext.
//
// Reading stops at whatever error r produces; because CTR cannot detect
// tampering, a truncated blob simply yields short plaintext rather than an
// error. Only the header is validated.
func (c *AESCipher) DecryptReader(r io.Reader) (io.Reader, error) {
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("crypto: reading blob header: %w", err)
	}

	if !bytes.Equal(header[:len(magic)], []byte(magic)) {
		return nil, fmt.Errorf(
			"crypto: not a weaveFS encrypted blob (magic was %q, want %q) — "+
				"was it written before encryption was enabled?",
			header[:len(magic)], magic)
	}

	if got := header[len(magic)]; got != formatVersion {
		return nil, fmt.Errorf(
			"crypto: unsupported blob format version 0x%02x, this build understands 0x%02x",
			got, formatVersion)
	}

	iv := header[len(magic)+1:]
	return &cipher.StreamReader{S: cipher.NewCTR(c.block, iv), R: r}, nil
}
