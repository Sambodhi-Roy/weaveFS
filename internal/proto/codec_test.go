package proto

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleMeta returns a fully populated VersionMeta, so round-trip tests prove
// every field survives rather than only the ones that happen to be set.
func sampleMeta() VersionMeta {
	return VersionMeta{
		VersionID: "11111111-2222-4333-8444-555555555555",
		Seq:       3,
		CreatedAt: time.Date(2026, 7, 26, 10, 30, 0, 0, time.UTC),
		SizeBytes: 47,
		BlobBytes: 68,
		Message:   "quarterly numbers",
	}
}

func TestRoundTripStoreRequest(t *testing.T) {
	want := StoreRequest{OriginID: "12D3KooWOrigin", Key: "report", Meta: sampleMeta()}

	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, want))

	var got StoreRequest
	require.NoError(t, ReadMessage(&buf, &got))
	assert.Equal(t, want, got)
}

func TestRoundTripStoreResponse(t *testing.T) {
	for _, want := range []StoreResponse{
		{OK: true},
		{OK: false, Error: "disk full"},
	} {
		var buf bytes.Buffer
		require.NoError(t, WriteMessage(&buf, want))

		var got StoreResponse
		require.NoError(t, ReadMessage(&buf, &got))
		assert.Equal(t, want, got)
	}
}

func TestRoundTripGetRequest(t *testing.T) {
	for _, want := range []GetRequest{
		{OriginID: "12D3KooWOrigin", Key: "report"},
		{OriginID: "12D3KooWOrigin", Key: "report", VersionID: "abc-123"},
	} {
		var buf bytes.Buffer
		require.NoError(t, WriteMessage(&buf, want))

		var got GetRequest
		require.NoError(t, ReadMessage(&buf, &got))
		assert.Equal(t, want, got)
	}
}

func TestRoundTripGetResponse(t *testing.T) {
	meta := sampleMeta()

	for _, want := range []GetResponse{
		{Found: true, Meta: &meta},
		{Found: false},
		{Found: false, Error: "not authorised"},
	} {
		var buf bytes.Buffer
		require.NoError(t, WriteMessage(&buf, want))

		var got GetResponse
		require.NoError(t, ReadMessage(&buf, &got))
		assert.Equal(t, want, got)
	}
}

// TestCreatedAtSurvivesAsUTC guards a subtlety of JSON time handling: a
// timestamp must come back as the same instant, and comparing time.Time values
// with == is sensitive to the location attached to them.
func TestCreatedAtSurvivesAsUTC(t *testing.T) {
	want := sampleMeta()

	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, want))

	var got VersionMeta
	require.NoError(t, ReadMessage(&buf, &got))

	assert.True(t, want.CreatedAt.Equal(got.CreatedAt), "the instant must be preserved")
	assert.Equal(t, want.CreatedAt, got.CreatedAt, "and so must the UTC location")
}

// TestSizeAndBlobBytesAreIndependent pins the reason the two fields exist
// separately: an encrypted blob is longer on the wire than the file it holds.
func TestSizeAndBlobBytesAreIndependent(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, sampleMeta()))

	var got VersionMeta
	require.NoError(t, ReadMessage(&buf, &got))

	assert.Equal(t, int64(47), got.SizeBytes, "SizeBytes is the plaintext length")
	assert.Equal(t, int64(68), got.BlobBytes, "BlobBytes is what actually crosses the wire")
}

// TestShortReadsAreHandled is the regression guard for the bug that killed the
// hand-written transport this package replaces.
//
// oneByteReader hands back a single byte per call, which is exactly what a
// network connection is allowed to do. The old decoder called Read once and
// treated whatever arrived as the complete message, silently truncating
// anything split across packets. ReadFull must loop instead.
func TestShortReadsAreHandled(t *testing.T) {
	want := StoreRequest{OriginID: "12D3KooWOrigin", Key: "report", Meta: sampleMeta()}

	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, want))

	var got StoreRequest
	require.NoError(t, ReadMessage(&oneByteReader{r: &buf}, &got),
		"a reader that dribbles out one byte at a time must still decode")
	assert.Equal(t, want, got)
}

// oneByteReader wraps a reader and never returns more than one byte per Read,
// simulating a network connection delivering data in fragments.
type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestOversizeHeaderRejected(t *testing.T) {
	// A frame claiming a header far larger than the limit, with no body behind
	// it. The read must be refused on the strength of the prefix alone, without
	// attempting the allocation.
	var frame bytes.Buffer
	var prefix [lengthPrefixSize]byte
	binary.BigEndian.PutUint32(prefix[:], maxHeaderBytes+1)
	frame.Write(prefix[:])

	var got StoreRequest
	err := ReadMessage(&frame, &got)

	require.Error(t, err, "an oversize header must be refused")
	assert.Contains(t, err.Error(), "over the")
}

func TestOversizeMessageRefusedOnWrite(t *testing.T) {
	huge := StoreRequest{Key: strings.Repeat("k", maxHeaderBytes+1)}

	err := WriteMessage(io.Discard, huge)

	require.Error(t, err, "we must not send what a peer would be right to reject")
	assert.Contains(t, err.Error(), "over the")
}

func TestTruncatedFrameErrors(t *testing.T) {
	t.Run("prefix cut short", func(t *testing.T) {
		var got StoreRequest
		err := ReadMessage(bytes.NewReader([]byte{0x00, 0x01}), &got)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "length prefix")
	})

	t.Run("body cut short", func(t *testing.T) {
		var full bytes.Buffer
		require.NoError(t, WriteMessage(&full, StoreRequest{Key: "report", Meta: sampleMeta()}))

		truncated := full.Bytes()[:full.Len()-10]

		var got StoreRequest
		err := ReadMessage(bytes.NewReader(truncated), &got)

		require.Error(t, err, "a body cut short must error, not return a partial value")
		assert.Contains(t, err.Error(), "header")
	})

	t.Run("empty stream", func(t *testing.T) {
		var got StoreRequest
		err := ReadMessage(bytes.NewReader(nil), &got)

		require.Error(t, err, "an empty stream must error rather than block or succeed")
	})
}

func TestMalformedJSONErrors(t *testing.T) {
	payload := []byte("{this is not json")

	var frame bytes.Buffer
	var prefix [lengthPrefixSize]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	frame.Write(prefix[:])
	frame.Write(payload)

	var got StoreRequest
	err := ReadMessage(&frame, &got)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

// TestUnknownFieldsIgnored proves forward compatibility: a newer node can add a
// field and an older node keeps working instead of refusing the message. This
// is the property that makes JSON worth its extra bytes here.
func TestUnknownFieldsIgnored(t *testing.T) {
	payload := []byte(`{"origin_id":"12D3KooWOrigin","key":"report","replication_factor":3}`)

	var frame bytes.Buffer
	var prefix [lengthPrefixSize]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	frame.Write(prefix[:])
	frame.Write(payload)

	var got GetRequest
	require.NoError(t, ReadMessage(&frame, &got),
		"a field this build has never heard of must be ignored, not fatal")

	assert.Equal(t, "12D3KooWOrigin", got.OriginID)
	assert.Equal(t, "report", got.Key)
}

// TestFrameLayoutIsBigEndian checks the bytes on the wire directly, since the
// layout is a contract with any future non-Go implementation and cannot be
// verified by round-tripping through our own code.
func TestFrameLayoutIsBigEndian(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, GetRequest{OriginID: "o", Key: "k"}))

	frame := buf.Bytes()
	require.Greater(t, len(frame), lengthPrefixSize)

	body := frame[lengthPrefixSize:]
	assert.Equal(t, uint32(len(body)), binary.BigEndian.Uint32(frame[:lengthPrefixSize]),
		"the prefix must be the body length, most significant byte first")

	// The two high-order bytes of a small length are zero in big-endian order.
	// Little-endian would put them at the other end, so this distinguishes them.
	assert.Equal(t, byte(0), frame[0])
	assert.Equal(t, byte(0), frame[1])
	assert.Equal(t, byte('{'), body[0], "the body should be JSON")
}

// TestSequentialMessagesOnOneStream covers the request/response pattern the
// handlers use: two messages written back to back must read back cleanly, with
// the second not disturbed by the first.
func TestSequentialMessagesOnOneStream(t *testing.T) {
	var buf bytes.Buffer

	require.NoError(t, WriteMessage(&buf, GetRequest{OriginID: "o", Key: "k"}))
	require.NoError(t, WriteMessage(&buf, StoreResponse{OK: true}))

	var req GetRequest
	require.NoError(t, ReadMessage(&buf, &req))
	assert.Equal(t, "k", req.Key)

	var resp StoreResponse
	require.NoError(t, ReadMessage(&buf, &resp))
	assert.True(t, resp.OK)
}

// TestBlobBytesFollowHeader models the real stream shape: a header, then file
// bytes that are not part of any frame. The reader must stop exactly at the end
// of the header and leave the blob untouched for the caller to copy.
func TestBlobBytesFollowHeader(t *testing.T) {
	blob := []byte("WFS1\x01" + strings.Repeat("ciphertext", 10))

	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, StoreRequest{Key: "k", Meta: sampleMeta()}))
	buf.Write(blob)

	var got StoreRequest
	require.NoError(t, ReadMessage(&buf, &got))
	assert.Equal(t, "k", got.Key)

	rest, err := io.ReadAll(&buf)
	require.NoError(t, err)
	assert.Equal(t, blob, rest,
		"the blob must survive intact, with the header read consuming not one byte of it")
}

func TestProtocolIDsAreVersioned(t *testing.T) {
	assert.Equal(t, "/weavefs/store/1.0.0", string(StoreProtocol))
	assert.Equal(t, "/weavefs/get/1.0.0", string(GetProtocol))
	assert.NotEqual(t, StoreProtocol, GetProtocol,
		"the two protocols must route to different handlers")
}
