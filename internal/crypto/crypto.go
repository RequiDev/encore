// Package crypto holds Encore's small set of cryptographic primitives: sealing
// Spotify tokens at rest, minting session tokens, and constant-time comparison.
//
// There is no password hashing here, deliberately: Encore has no local passwords.
// Identity comes from Spotify alone.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrDecrypt is returned for any failure to open a sealed value. It is
// deliberately uninformative: distinguishing "wrong key" from "corrupt
// ciphertext" leaks information to anyone who can trigger a decryption.
var ErrDecrypt = errors.New("could not decrypt value")

// Sealer encrypts and decrypts values at rest with AES-256-GCM.
//
// The sealed form is nonce || ciphertext || tag. A fresh random nonce is drawn
// per operation, so sealing the same plaintext twice never produces the same
// bytes and an attacker with database access learns nothing from equality.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer builds a Sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build GCM: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext. An empty input seals to an empty output so that
// "no token" round-trips as "no token" rather than as ciphertext.
func (s *Sealer) Seal(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return []byte{}, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a value produced by Seal.
func (s *Sealer) Open(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return "", ErrDecrypt
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

// TokenBytes is the entropy of a session or state token. 32 bytes is well past
// any brute-force concern and encodes to 43 URL-safe characters.
const TokenBytes = 32

// NewToken mints a URL-safe random token.
func NewToken() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the one-way transform applied before a token is stored. The
// database holds only this, so a database leak cannot be replayed as a login.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// EqualTokens compares two tokens in constant time.
func EqualTokens(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// EqualBytes compares two byte slices in constant time.
func EqualBytes(a, b []byte) bool { return hmac.Equal(a, b) }

// PKCEVerifier mints an RFC 7636 code verifier.
func PKCEVerifier() (string, error) { return NewToken() }

// PKCEChallenge derives the S256 code challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
