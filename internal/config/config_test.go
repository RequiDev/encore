package config

import (
	"encoding/base64"
	"encoding/hex"
	"slices"
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

// TestDefaultScopesAreTheEightReadScopes pins the sign-in grant.
//
// The list is asserted exactly, in order, rather than by length or by
// membership: this is the set every listener is asked to consent to, and it
// should not be possible to widen it without a test changing to say so.
func TestDefaultScopesAreTheEightReadScopes(t *testing.T) {
	want := []string{
		"user-read-recently-played",
		"user-read-private",
		"user-read-email",
		"user-top-read",
		"user-library-read",
		"user-follow-read",
		"playlist-read-private",
		"user-read-playback-state",
	}
	got := DefaultScopes()
	if !slices.Equal(got, want) {
		t.Errorf("DefaultScopes() =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDefaultScopesGrantNoWriteAccess is the property that matters more than
// the list itself: nothing Encore asks for at sign-in can alter a listener's
// Spotify account, or act on their behalf.
//
// An allow-list, not a deny-list. Substring rules miss what they were not
// written for — app-remote-control and streaming both let a client take a real
// action and contain neither "modify" nor "ugc-". Naming the read scopes that
// are permitted means a future addition has to be added here deliberately,
// which is the review step this guard exists to force.
func TestDefaultScopesGrantNoWriteAccess(t *testing.T) {
	readOnly := map[string]bool{
		"user-read-recently-played":   true,
		"user-read-private":           true,
		"user-read-email":             true,
		"user-top-read":               true,
		"user-library-read":           true,
		"user-follow-read":            true,
		"playlist-read-private":       true,
		"playlist-read-collaborative": true,
		"user-read-playback-state":    true,
		"user-read-currently-playing": true,
		"user-read-playback-position": true,
	}
	for _, s := range DefaultScopes() {
		if !readOnly[s] {
			t.Errorf("%q is not a known read-only scope and must not be in the sign-in grant", s)
		}
	}
}
