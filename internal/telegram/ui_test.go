package telegram

import (
	"strings"
	"testing"
)

const txHash = "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestLooksLikeTaskInput(t *testing.T) {
	yes := []string{
		"https://opensea.io/collection/dog-852994405",
		"https://opensea.io/item/ethereum/0x1234567890abcdef1234567890abcdef12345678/1",
		"0x1234567890abcdef1234567890abcdef12345678",
		"  0x1234567890ABCDEF1234567890ABCDEF12345678  ",
		txHash, // bare tx hash
		"https://etherscan.io/tx/" + txHash,
		"https://polygonscan.com/address/0x1234567890abcdef1234567890abcdef12345678",
	}
	no := []string{"/status", "hello", "buy 5 eth", "dog-852994405"}
	for _, s := range yes {
		if !looksLikeTaskInput(s) {
			t.Errorf("looksLikeTaskInput(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeTaskInput(s) {
			t.Errorf("looksLikeTaskInput(%q) = true, want false", s)
		}
	}
}

func TestIsTxHash(t *testing.T) {
	if !isTxHash(txHash) {
		t.Errorf("isTxHash(%q) = false, want true", txHash)
	}
	for _, s := range []string{
		"0x1234567890abcdef1234567890abcdef12345678", // 40 hex = address, not a hash
		"0xzz" + strings.Repeat("0", 62),             // non-hex
		txHash + "00",                                // too long
	} {
		if isTxHash(s) {
			t.Errorf("isTxHash(%q) = true, want false", s)
		}
	}
}

func TestParseNftLink(t *testing.T) {
	const addr = "0x1234567890abcdef1234567890abcdef12345678"
	cases := []struct {
		in          string
		wantSlug    string
		wantHasAddr bool
		wantChain   int
	}{
		{"https://opensea.io/collection/dog-852994405", "dog-852994405", false, 1},
		{"https://opensea.io/collection/dog-852994405/overview?tab=items", "dog-852994405", false, 1},
		{"https://opensea.io/item/ethereum/" + addr + "/7", "", true, 1},
		{"https://opensea.io/item/matic/" + addr + "/7", "", true, 137},
		{addr, "", true, 1},
		{"some-bare-slug", "some-bare-slug", false, 1},
	}
	for _, c := range cases {
		contract, slug, chain := parseNftLink(c.in)
		if slug != c.wantSlug {
			t.Errorf("parseNftLink(%q) slug = %q, want %q", c.in, slug, c.wantSlug)
		}
		hasAddr := strings.HasPrefix(strings.ToLower(contract), "0x")
		if hasAddr != c.wantHasAddr {
			t.Errorf("parseNftLink(%q) hasAddr = %v, want %v (contract=%q)", c.in, hasAddr, c.wantHasAddr, contract)
		}
		if chain != c.wantChain {
			t.Errorf("parseNftLink(%q) chain = %d, want %d", c.in, chain, c.wantChain)
		}
	}
}

func TestParseExplorerURL(t *testing.T) {
	const addr = "0x1234567890abcdef1234567890abcdef12345678"
	cases := []struct {
		in        string
		wantKind  string
		wantVal   string
		wantChain int
		wantOK    bool
	}{
		{"https://etherscan.io/tx/" + txHash, "tx", txHash, 1, true},
		{"https://sepolia.etherscan.io/tx/" + txHash, "tx", txHash, 11155111, true}, // longest-domain wins
		{"https://polygonscan.com/address/" + addr, "addr", addr, 137, true},
		{"https://basescan.org/token/" + addr + "?a=1", "addr", addr, 8453, true},
		{"https://opensea.io/collection/dog", "", "", 0, false}, // not an explorer
	}
	for _, c := range cases {
		kind, val, chain, ok := parseExplorerURL(c.in)
		if ok != c.wantOK || kind != c.wantKind || chain != c.wantChain ||
			(c.wantOK && !strings.EqualFold(val, c.wantVal)) {
			t.Errorf("parseExplorerURL(%q) = (%q,%q,%d,%v), want (%q,%q,%d,%v)",
				c.in, kind, val, chain, ok, c.wantKind, c.wantVal, c.wantChain, c.wantOK)
		}
	}
}
