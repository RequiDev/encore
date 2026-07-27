package config

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// TestEncryptionKeyAcceptsHexAndBase64 pins a distinction that is easy to get
// backwards: a 64-character hex key is also syntactically valid base64, and
// decoding it that way yields 48 bytes. Preferring whichever decoding produces a
// usable key is what makes both documented forms work.
func TestEncryptionKeyAcceptsHexAndBase64(t *testing.T) {
	raw := make([]byte, KeyBytes)
	for i := range raw {
		raw[i] = byte(i * 7)
	}

	for name, encoded := range map[string]string{
		"hex":             hex.EncodeToString(raw),
		"base64 standard": base64.StdEncoding.EncodeToString(raw),
		"base64 raw":      base64.RawStdEncoding.EncodeToString(raw),
		"base64 url-safe": base64.URLEncoding.EncodeToString(raw),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := decodeKey(encoded)
			if err != nil {
				t.Fatalf("decode %s key: %v", name, err)
			}
			if len(got) != KeyBytes {
				t.Fatalf("%s key decoded to %d bytes, want %d", name, len(got), KeyBytes)
			}
			if string(got) != string(raw) {
				t.Fatalf("%s key round-tripped to different bytes", name)
			}
		})
	}
}

// TestEncryptionKeyRejectsTheWrongLength keeps the error message useful: a key
// that decodes but is the wrong size must report its size, not a parse failure.
func TestEncryptionKeyRejectsTheWrongLength(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	got, err := decodeKey(short)
	if err != nil {
		t.Fatalf("a decodable but short key should decode, then be rejected on length: %v", err)
	}
	if len(got) == KeyBytes {
		t.Fatal("a 16-byte key must not be reported as 32 bytes")
	}
}
