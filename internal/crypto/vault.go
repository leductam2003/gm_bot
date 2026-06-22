// Package crypto implements the at-rest vault: a master password is stretched
// with Argon2id into a 256-bit key, and every secret (private key) is sealed
// with AES-256-GCM. The plaintext key never touches disk; only ciphertext does.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Tuned for an interactive unlock on a VPS.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	keyLen       = 32 // AES-256
	saltLen      = 16
)

// verifierPlaintext is sealed at init and re-opened at unlock to prove the
// supplied password derives the same key (without storing the password).
var verifierPlaintext = []byte("zyperbot-vault-v1")

var (
	ErrLocked       = errors.New("vault is locked")
	ErrBadPassword  = errors.New("wrong master password")
	ErrNotInitDone  = errors.New("vault not initialized")
	ErrAlreadyInit  = errors.New("vault already initialized")
)

// Vault holds the derived key in memory after a successful unlock.
type Vault struct {
	mu  sync.RWMutex
	key []byte // nil when locked
}

func New() *Vault { return &Vault{} }

// deriveKey stretches the password with the stored salt.
func deriveKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyLen)
}

// InitParams is what the store persists so a vault can be unlocked later.
type InitParams struct {
	Salt     string // hex
	Verifier string // hex sealed verifierPlaintext
}

// Init creates fresh salt + verifier from a new master password. Call once.
func Init(password string) (InitParams, error) {
	if len(password) < 8 {
		return InitParams{}, errors.New("master password must be at least 8 characters")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return InitParams{}, err
	}
	key := deriveKey(password, salt)
	sealed, err := seal(key, verifierPlaintext)
	if err != nil {
		return InitParams{}, err
	}
	return InitParams{Salt: hex.EncodeToString(salt), Verifier: hex.EncodeToString(sealed)}, nil
}

// Unlock verifies the password against the stored params and, on success,
// keeps the derived key in memory.
func (v *Vault) Unlock(password string, p InitParams) error {
	salt, err := hex.DecodeString(p.Salt)
	if err != nil {
		return fmt.Errorf("bad salt: %w", err)
	}
	sealed, err := hex.DecodeString(p.Verifier)
	if err != nil {
		return fmt.Errorf("bad verifier: %w", err)
	}
	key := deriveKey(password, salt)
	opened, err := open(key, sealed)
	ok := err == nil && subtle.ConstantTimeCompare(opened, verifierPlaintext) == 1
	// Wipe the decrypted verifier bytes regardless of outcome.
	for i := range opened {
		opened[i] = 0
	}
	if !ok {
		// Wipe the wrongly-derived key so it doesn't linger in memory.
		for i := range key {
			key[i] = 0
		}
		return ErrBadPassword
	}
	v.mu.Lock()
	v.key = key
	v.mu.Unlock()
	return nil
}

// Lock wipes the in-memory key.
func (v *Vault) Lock() {
	v.mu.Lock()
	for i := range v.key {
		v.key[i] = 0
	}
	v.key = nil
	v.mu.Unlock()
}

// Unlocked reports whether the key is currently held.
func (v *Vault) Unlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.key != nil
}

// Seal encrypts a secret with the in-memory key. Returns hex(nonce||ciphertext).
func (v *Vault) Seal(plaintext []byte) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.key == nil {
		return "", ErrLocked
	}
	out, err := seal(v.key, plaintext)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(out), nil
}

// Open decrypts a hex(nonce||ciphertext) blob produced by Seal.
func (v *Vault) Open(hexBlob string) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.key == nil {
		return nil, ErrLocked
	}
	raw, err := hex.DecodeString(hexBlob)
	if err != nil {
		return nil, err
	}
	return open(v.key, raw)
}

// --- low-level AES-256-GCM helpers (nonce is prepended to the ciphertext) ---

func seal(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func open(key, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, blob[:ns], blob[ns:], nil)
}
