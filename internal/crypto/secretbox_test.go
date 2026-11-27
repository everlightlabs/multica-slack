package crypto_test

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/everlightlabs/multica-slack/internal/crypto"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

func TestBoxSealOpenRoundTrip(t *testing.T) {
	key := testKey(t)
	box, err := crypto.NewBox(key)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	plaintext := "mul_secret_123"
	ct1, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	ct2, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}

	if !strings.HasPrefix(ct1, "v1:") {
		t.Errorf("ciphertext %q missing v1: prefix", ct1)
	}
	parts := strings.Split(ct1, ":")
	if len(parts) != 4 {
		t.Errorf("ciphertext has %d segments, want 4", len(parts))
	}
	if strings.Contains(ct1, plaintext) {
		t.Errorf("ciphertext %q must not contain plaintext", ct1)
	}
	if ct1 == ct2 {
		t.Errorf("two seals produced identical ciphertexts (expected random 12-byte IV)")
	}

	got1, err := box.Open(ct1)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if got1 != plaintext {
		t.Errorf("Open 1 = %q, want %q", got1, plaintext)
	}

	got2, err := box.Open(ct2)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if got2 != plaintext {
		t.Errorf("Open 2 = %q, want %q", got2, plaintext)
	}
}

func TestBoxRejectsTamperedAndWrongKey(t *testing.T) {
	key1 := testKey(t)
	key2 := testKey(t)

	for _, badLen := range []int{0, 16, 31, 33, 64} {
		if _, err := crypto.NewBox(make([]byte, badLen)); err == nil {
			t.Fatalf("NewBox(%d bytes) expected error, got nil", badLen)
		}
	}

	box1, err := crypto.NewBox(key1)
	if err != nil {
		t.Fatalf("NewBox 1: %v", err)
	}
	box2, err := crypto.NewBox(key2)
	if err != nil {
		t.Fatalf("NewBox 2: %v", err)
	}

	ct, err := box1.Seal("mul_secret_123")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := box2.Open(ct); err == nil {
		t.Fatal("Open with wrong key expected error, got nil")
	}

	// Flip a character in the ciphertext payload segment.
	parts := strings.Split(ct, ":")
	ctBytes := []byte(parts[3])
	if ctBytes[0] == 'A' {
		ctBytes[0] = 'B'
	} else {
		ctBytes[0] = 'A'
	}
	parts[3] = string(ctBytes)
	tampered := strings.Join(parts, ":")

	if _, err := box1.Open(tampered); err == nil {
		t.Fatal("Open with tampered ciphertext expected error, got nil")
	}

	for _, malformed := range []string{"", "v1:abc", "v2:a:b:c", "v1:!!!:b:c"} {
		if _, err := box1.Open(malformed); err == nil {
			t.Fatalf("Open(%q) expected error, got nil", malformed)
		}
	}
}

func TestStartTokenSignVerify(t *testing.T) {
	key := testKey(t)
	otherKey := testKey(t)
	now := time.Unix(1700000000, 0).UTC()
	ttl := 10 * time.Minute

	token := crypto.SignStartToken(key, "U01234567", now)
	if token == "" {
		t.Fatal("SignStartToken returned empty string")
	}

	// Valid within TTL.
	gotUser, err := crypto.VerifyStartToken(key, token, now.Add(5*time.Minute), ttl)
	if err != nil {
		t.Fatalf("VerifyStartToken valid: %v", err)
	}
	if gotUser != "U01234567" {
		t.Errorf("got user %q, want %q", gotUser, "U01234567")
	}

	// Past TTL.
	if _, err := crypto.VerifyStartToken(key, token, now.Add(11*time.Minute), ttl); err == nil {
		t.Fatal("VerifyStartToken past TTL expected error, got nil")
	}

	// Tampered token (both trailing char / strict base64 bits and mid-signature char).
	tampered := token[:len(token)-1] + "X"
	if tampered == token {
		tampered = token[:len(token)-1] + "Y"
	}
	if _, err := crypto.VerifyStartToken(key, tampered, now.Add(time.Minute), ttl); err == nil {
		t.Fatal("VerifyStartToken tampered trailing char expected error, got nil")
	}
	midIdx := len(token) - 10
	midChar := "A"
	if token[midIdx] == 'A' {
		midChar = "B"
	}
	tamperedMid := token[:midIdx] + midChar + token[midIdx+1:]
	if _, err := crypto.VerifyStartToken(key, tamperedMid, now.Add(time.Minute), ttl); err == nil {
		t.Fatal("VerifyStartToken tampered mid-signature expected error, got nil")
	}

	// Wrong key.
	if _, err := crypto.VerifyStartToken(otherKey, token, now.Add(time.Minute), ttl); err == nil {
		t.Fatal("VerifyStartToken wrong key expected error, got nil")
	}

	// Future token (issued after `now`).
	if _, err := crypto.VerifyStartToken(key, token, now.Add(-1*time.Minute), ttl); err == nil {
		t.Fatal("VerifyStartToken before issuedAt expected error, got nil")
	}
}
