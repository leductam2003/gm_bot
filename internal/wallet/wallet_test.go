package wallet

import (
	"strings"
	"testing"
)

func TestGenerateValidAddress(t *testing.T) {
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Address, "0x") || len(k.Address) != 42 {
		t.Fatalf("bad address: %s", k.Address)
	}
	if !strings.HasPrefix(k.PrivKeyHex, "0x") || len(k.PrivKeyHex) != 66 {
		t.Fatalf("bad priv key length: %s", k.PrivKeyHex)
	}
}

func TestImportRoundtrip(t *testing.T) {
	k, _ := Generate()
	k2, err := Import(k.PrivKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if k2.Address != k.Address {
		t.Fatalf("import gave different address: %s != %s", k2.Address, k.Address)
	}
}

func TestImportKnownVector(t *testing.T) {
	// Well-known test key → address (Hardhat account #0).
	const pk = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	const want = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	k, err := Import(pk)
	if err != nil {
		t.Fatal(err)
	}
	if k.Address != want {
		t.Fatalf("got %s want %s", k.Address, want)
	}
}

func TestImportInvalid(t *testing.T) {
	if _, err := Import("not-a-key"); err == nil {
		t.Fatal("expected error for invalid key")
	}
}
