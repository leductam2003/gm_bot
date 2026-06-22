// Package wallet generates and imports EVM key pairs. It works only with the
// derived plaintext key in memory; the caller seals the private key via the vault
// before persisting. (Solana/Bitcoin are stubbed for a later phase.)
package wallet

import (
	"crypto/ecdsa"
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Key is a freshly generated or imported EVM key with its plaintext private key.
type Key struct {
	Address    string // EIP-55 checksummed
	PrivKeyHex string // 0x-prefixed, 64 hex chars
}

// Generate creates a new random secp256k1 key.
func Generate() (Key, error) {
	pk, err := crypto.GenerateKey()
	if err != nil {
		return Key{}, err
	}
	return fromECDSA(pk), nil
}

// GenerateN creates n random keys.
func GenerateN(n int) ([]Key, error) {
	if n < 1 || n > 1000 {
		return nil, errors.New("count must be between 1 and 1000")
	}
	out := make([]Key, 0, n)
	for i := 0; i < n; i++ {
		k, err := Generate()
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// Import parses a 0x-prefixed (or bare) private key hex into a Key.
func Import(privHex string) (Key, error) {
	clean := strings.TrimSpace(privHex)
	clean = strings.TrimPrefix(clean, "0x")
	pk, err := crypto.HexToECDSA(clean)
	if err != nil {
		return Key{}, errors.New("invalid private key")
	}
	return fromECDSA(pk), nil
}

// AddressFromPriv recovers the checksummed address from a 0x-prefixed key.
func AddressFromPriv(privHex string) (string, error) {
	k, err := Import(privHex)
	if err != nil {
		return "", err
	}
	return k.Address, nil
}

func fromECDSA(pk *ecdsa.PrivateKey) Key {
	addr := crypto.PubkeyToAddress(pk.PublicKey)
	return Key{
		Address:    common.BytesToAddress(addr.Bytes()).Hex(),
		PrivKeyHex: "0x" + common.Bytes2Hex(crypto.FromECDSA(pk)),
	}
}
