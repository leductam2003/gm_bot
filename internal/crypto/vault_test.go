package crypto

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundtrip(t *testing.T) {
	p, err := Init("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	v := New()
	if err := v.Unlock("correct horse battery", p); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	secret := []byte("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	blob, err := v.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("roundtrip mismatch")
	}
}

func TestWrongPassword(t *testing.T) {
	p, err := Init("right-password")
	if err != nil {
		t.Fatal(err)
	}
	v := New()
	if err := v.Unlock("wrong-password", p); err != ErrBadPassword {
		t.Fatalf("expected ErrBadPassword, got %v", err)
	}
}

func TestLockedSealFails(t *testing.T) {
	v := New()
	if _, err := v.Seal([]byte("x")); err != ErrLocked {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestLockWipes(t *testing.T) {
	p, _ := Init("a-good-password")
	v := New()
	_ = v.Unlock("a-good-password", p)
	v.Lock()
	if v.Unlocked() {
		t.Fatal("still unlocked after Lock")
	}
}

func TestDifferentNonces(t *testing.T) {
	p, _ := Init("nonce-password")
	v := New()
	_ = v.Unlock("nonce-password", p)
	a, _ := v.Seal([]byte("same"))
	b, _ := v.Seal([]byte("same"))
	if a == b {
		t.Fatal("identical ciphertext for same plaintext — nonce not random")
	}
}
