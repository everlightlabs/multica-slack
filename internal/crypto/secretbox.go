package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	cipherVersion      = "v1"
	requiredKeyBytes   = 32
	startTokenKeyLabel = "oauth-start:"
)

// Box encrypts and decrypts sensitive strings using AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// NewBox constructs an AES-256-GCM Box from a 32-byte key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != requiredKeyBytes {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", requiredKeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext with a random 12-byte nonce and encodes the result as
// v1:<iv_b64>:<tag_b64>:<ct_b64>.
func (b *Box) Seal(plaintext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("crypto: uninitialized Box")
	}

	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}

	sealed := b.aead.Seal(nil, nonce, []byte(plaintext), nil)
	tagOffset := len(sealed) - b.aead.Overhead()
	ct := sealed[:tagOffset]
	tag := sealed[tagOffset:]

	return cipherVersion + ":" +
		base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(tag) + ":" +
		base64.StdEncoding.EncodeToString(ct), nil
}

// Open decrypts and authenticates a v1:<iv_b64>:<tag_b64>:<ct_b64> ciphertext.
func (b *Box) Open(ciphertext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("crypto: uninitialized Box")
	}

	parts := strings.Split(ciphertext, ":")
	if len(parts) != 4 || parts[0] != cipherVersion {
		return "", errors.New("crypto: invalid ciphertext format")
	}

	nonce, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("crypto: decode nonce: %w", err)
	}
	if len(nonce) != b.aead.NonceSize() {
		return "", errors.New("crypto: invalid nonce length")
	}

	tag, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("crypto: decode tag: %w", err)
	}
	if len(tag) != b.aead.Overhead() {
		return "", errors.New("crypto: invalid tag length")
	}

	ct, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return "", fmt.Errorf("crypto: decode ciphertext: %w", err)
	}

	combined := make([]byte, 0, len(ct)+len(tag))
	combined = append(combined, ct...)
	combined = append(combined, tag...)

	plaintext, err := b.aead.Open(nil, nonce, combined, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// SignStartToken creates a short-lived HMAC-SHA256 token for Slack OAuth start links.
func SignStartToken(key []byte, slackUserID string, now time.Time) string {
	payload := slackUserID + "." + strconv.FormatInt(now.Unix(), 10)
	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))

	mac := hmac.New(sha256.New, deriveStartTokenKey(key))
	mac.Write([]byte(payloadB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return payloadB64 + "." + sigB64
}

// VerifyStartToken verifies a token signed by SignStartToken and checks its TTL.
func VerifyStartToken(key []byte, token string, now time.Time, ttl time.Duration) (string, error) {
	if len(key) == 0 {
		return "", errors.New("crypto: empty signing key")
	}

	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok || payloadB64 == "" || sigB64 == "" {
		return "", errors.New("crypto: invalid start token format")
	}

	gotSig, err := base64.RawURLEncoding.Strict().DecodeString(sigB64)
	if err != nil {
		return "", fmt.Errorf("crypto: decode start token signature: %w", err)
	}

	mac := hmac.New(sha256.New, deriveStartTokenKey(key))
	mac.Write([]byte(payloadB64))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return "", errors.New("crypto: invalid start token signature")
	}

	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(payloadB64)
	if err != nil {
		return "", fmt.Errorf("crypto: decode start token payload: %w", err)
	}

	payload := string(payloadBytes)
	dot := strings.LastIndexByte(payload, '.')
	if dot <= 0 {
		return "", errors.New("crypto: invalid start token payload")
	}

	slackUserID := payload[:dot]
	unixSec, err := strconv.ParseInt(payload[dot+1:], 10, 64)
	if err != nil {
		return "", fmt.Errorf("crypto: invalid start token timestamp: %w", err)
	}

	age := now.Sub(time.Unix(unixSec, 0))
	if age < 0 || age > ttl {
		return "", errors.New("crypto: start token expired")
	}

	return slackUserID, nil
}

func deriveStartTokenKey(key []byte) []byte {
	h := sha256.New()
	h.Write([]byte(startTokenKeyLabel))
	h.Write(key)
	return h.Sum(nil)
}
