package crypto

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKey returns a deterministic key of the right length. Tests that care
// about randomness generate their own; the rest just need something valid.
func testKey(t *testing.T, fill byte) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = fill
	}
	return key
}

// encrypt is a convenience wrapper: run plaintext through a cipher and return
// the resulting blob bytes.
func encrypt(t *testing.T, c *AESCipher, plaintext []byte) []byte {
	t.Helper()

	var blob bytes.Buffer
	w, err := c.EncryptWriter(&blob)
	require.NoError(t, err, "EncryptWriter must succeed")

	_, err = io.Copy(w, bytes.NewReader(plaintext))
	require.NoError(t, err, "writing plaintext must succeed")

	return blob.Bytes()
}

// decrypt is the mirror of encrypt: run a blob back through a cipher.
func decrypt(t *testing.T, c *AESCipher, blob []byte) ([]byte, error) {
	t.Helper()

	r, err := c.DecryptReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0xA5))
	require.NoError(t, err)

	plaintext := []byte("weaveFS: distributed storage, one node at a time")

	got, err := decrypt(t, c, encrypt(t, c, plaintext))
	require.NoError(t, err)
	assert.Equal(t, plaintext, got, "decrypted content should match the original")
}

// TestRoundTripAcrossSizes covers the edges CTR is most likely to get wrong:
// empty input, a single byte, and lengths either side of the 16-byte AES block
// boundary. CTR should not care about block alignment — this proves it.
func TestRoundTripAcrossSizes(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x3C))
	require.NoError(t, err)

	for _, size := range []int{0, 1, 15, 16, 17, 1024, 64 * 1024} {
		plaintext := bytes.Repeat([]byte("x"), size)

		blob := encrypt(t, c, plaintext)
		assert.Len(t, blob, size+headerSize,
			"a %d-byte plaintext should produce a %d-byte blob", size, size+headerSize)

		got, err := decrypt(t, c, blob)
		require.NoError(t, err)
		assert.Equal(t, plaintext, got, "round trip failed at size %d", size)
	}
}

func TestCiphertextDiffersFromPlaintext(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x11))
	require.NoError(t, err)

	plaintext := []byte("the quick brown fox jumps over the lazy dog")
	blob := encrypt(t, c, plaintext)

	assert.False(t, bytes.Contains(blob, plaintext),
		"the blob must not contain the plaintext anywhere")
	assert.True(t, bytes.HasPrefix(blob, []byte(magic)),
		"the blob must start with the %q magic", magic)
}

// TestIVIsUniquePerEncryption guards the single most dangerous mistake
// available in CTR mode. Reusing an IV under one key lets an attacker XOR two
// ciphertexts together and recover the XOR of the plaintexts, with the key
// cancelling out. Identical plaintext must therefore produce different bytes
// every time.
func TestIVIsUniquePerEncryption(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x77))
	require.NoError(t, err)

	plaintext := []byte("same message, encrypted twice")
	first := encrypt(t, c, plaintext)
	second := encrypt(t, c, plaintext)

	assert.NotEqual(t, first, second, "two encryptions of the same plaintext must differ")

	ivStart := len(magic) + 1
	assert.NotEqual(t, first[ivStart:ivStart+ivSize], second[ivStart:ivStart+ivSize],
		"each blob must carry its own freshly generated IV")
}

// TestWrongKeyDoesNotRecoverPlaintext documents an accepted limitation rather
// than a desirable property. CTR provides confidentiality but not integrity, so
// decrypting under the wrong key yields garbage and reports NO error. Asserting
// NoError here is deliberate: if a future change adds authentication, this test
// fails and forces the decision to be made consciously.
func TestWrongKeyDoesNotRecoverPlaintext(t *testing.T) {
	writer, err := NewAESCipher(testKey(t, 0x01))
	require.NoError(t, err)
	reader, err := NewAESCipher(testKey(t, 0x02))
	require.NoError(t, err)

	plaintext := []byte("a secret worth keeping")
	got, err := decrypt(t, reader, encrypt(t, writer, plaintext))

	require.NoError(t, err, "CTR cannot detect a wrong key — see decision 5")
	assert.NotEqual(t, plaintext, got, "the wrong key must not recover the plaintext")
	assert.Len(t, got, len(plaintext), "CTR preserves length even when the key is wrong")
}

// TestDecryptRejectsPlaintextBlob is the reason the header exists. Without it,
// this input would decrypt to silent garbage with a nil error.
func TestDecryptRejectsPlaintextBlob(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x5F))
	require.NoError(t, err)

	plaintext := []byte("this blob was written before encryption was enabled, in plaintext")

	_, err = c.DecryptReader(bytes.NewReader(plaintext))
	require.Error(t, err, "a plaintext blob must be rejected, not silently mangled")
	assert.Contains(t, err.Error(), "not a weaveFS encrypted blob")
}

func TestDecryptRejectsTruncatedHeader(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x5F))
	require.NoError(t, err)

	// Correct magic and version, but the IV is cut short.
	truncated := append([]byte(magic), formatVersion, 0x00, 0x01, 0x02)

	_, err = c.DecryptReader(bytes.NewReader(truncated))
	require.Error(t, err, "a blob shorter than the header must error, not panic")
	assert.Contains(t, err.Error(), "reading blob header")
}

func TestDecryptRejectsEmptyInput(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x5F))
	require.NoError(t, err)

	_, err = c.DecryptReader(bytes.NewReader(nil))
	assert.Error(t, err, "an empty blob must error, not panic")
}

func TestDecryptRejectsUnknownFormatVersion(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x5F))
	require.NoError(t, err)

	// Right magic, a version this build has never heard of, valid-length IV.
	blob := append([]byte(magic), 0xFF)
	blob = append(blob, make([]byte, ivSize)...)

	_, err = c.DecryptReader(bytes.NewReader(blob))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported blob format version")
}

func TestNewAESCipherRejectsBadKeyLength(t *testing.T) {
	// 16 bytes is a valid AES-128 key, but weaveFS is AES-256 only, so it must
	// still be rejected rather than quietly weakening the cipher.
	for _, size := range []int{0, 16, 31, 33, 64} {
		_, err := NewAESCipher(make([]byte, size))
		assert.Error(t, err, "a %d-byte key must be rejected", size)
	}
}

func TestOverheadMatchesActualBlobGrowth(t *testing.T) {
	c, err := NewAESCipher(testKey(t, 0x9E))
	require.NoError(t, err)

	plaintext := []byte("measure me")
	blob := encrypt(t, c, plaintext)

	assert.Equal(t, int64(len(blob)-len(plaintext)), c.Overhead(),
		"Overhead() must equal the real difference, since store relies on it")
}

func TestLoadOrCreateKeyCreatesFile(t *testing.T) {
	// t.TempDir gives this test its own directory, removed automatically when
	// the test finishes, so tests cannot interfere via the filesystem.
	dir := t.TempDir()

	key, err := LoadOrCreateKey(dir)
	require.NoError(t, err)
	assert.Len(t, key, KeySize)

	raw, err := os.ReadFile(filepath.Join(dir, keyFileName))
	require.NoError(t, err, "the key file must exist after LoadOrCreateKey")

	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	require.NoError(t, err, "the key file must be valid hex")
	assert.Equal(t, key, decoded, "the file must hold the key that was returned")
}

func TestLoadOrCreateKeyPersists(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateKey(dir)
	require.NoError(t, err)

	second, err := LoadOrCreateKey(dir)
	require.NoError(t, err)

	assert.Equal(t, first, second,
		"a second call must load the existing key, not generate a new one — "+
			"generating one would make every stored blob unreadable")
}

func TestLoadOrCreateKeyGeneratesDistinctKeys(t *testing.T) {
	a, err := LoadOrCreateKey(t.TempDir())
	require.NoError(t, err)
	b, err := LoadOrCreateKey(t.TempDir())
	require.NoError(t, err)

	assert.NotEqual(t, a, b, "two fresh data directories must get different keys")
}

func TestLoadOrCreateKeyCreatesMissingDirectory(t *testing.T) {
	// WriteFileAtomic creates parent directories, so pointing at a data dir
	// that does not exist yet should work rather than failing on first run.
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")

	key, err := LoadOrCreateKey(dir)
	require.NoError(t, err)
	assert.Len(t, key, KeySize)
}

func TestLoadKeyRejectsCorruptFile(t *testing.T) {
	cases := map[string]string{
		"not hex at all":       "this is definitely not a hex encoded key!!",
		"valid hex, too short": hex.EncodeToString(make([]byte, 16)),
		"valid hex, too long":  hex.EncodeToString(make([]byte, 64)),
		"empty file":           "",
	}

	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(
				filepath.Join(dir, keyFileName), []byte(contents), keyFileMode))

			_, err := LoadOrCreateKey(dir)
			require.Error(t, err, "a corrupt key file must not be silently replaced")
			assert.Contains(t, err.Error(), "is corrupt")
		})
	}
}

func TestLoadKeyToleratesTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t, 0x42)

	// An editor opening the key file will typically add a trailing newline.
	// That should not brick the node.
	require.NoError(t, os.WriteFile(filepath.Join(dir, keyFileName),
		[]byte(hex.EncodeToString(key)+"\n"), keyFileMode))

	loaded, err := LoadOrCreateKey(dir)
	require.NoError(t, err)
	assert.Equal(t, key, loaded)
}

// TestKeyFileIsPrivate is skipped on Windows, where Unix permission bits are
// not enforced — os.Chmod there only toggles the read-only attribute, so the
// assertion would pass while proving nothing. weaveFS is developed on Windows,
// so this is stated plainly: the test is skipped rather than made to lie.
func TestKeyFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced on Windows")
	}

	dir := t.TempDir()
	_, err := LoadOrCreateKey(dir)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dir, keyFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(keyFileMode), info.Mode().Perm(),
		"the key file must be readable only by its owner")
}
