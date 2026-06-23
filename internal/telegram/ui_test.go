package telegram

import (
	"strings"
	"testing"
)

func TestLooksLikeNftLink(t *testing.T) {
	yes := []string{
		"https://opensea.io/collection/dog-852994405",
		"https://opensea.io/item/ethereum/0x1234567890abcdef1234567890abcdef12345678/1",
		"0x1234567890abcdef1234567890abcdef12345678",
		"  0x1234567890ABCDEF1234567890ABCDEF12345678  ",
	}
	no := []string{"/status", "hello", "buy 5 eth", "dog-852994405"}
	for _, s := range yes {
		if !looksLikeNftLink(s) {
			t.Errorf("looksLikeNftLink(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeNftLink(s) {
			t.Errorf("looksLikeNftLink(%q) = true, want false", s)
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
