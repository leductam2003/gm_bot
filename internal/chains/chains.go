// Package chains is the chain registry, ported from zyper-mac/src/chains.ts.
// RPC defaults mirror the public endpoints the original shipped with; users
// override them per chain via the RPC page.
package chains

import (
	"fmt"
	"strings"
)

type ChainInfo struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Symbol   string   `json:"symbol"`
	RPCs     []string `json:"rpcs"`
	Explorer string   `json:"explorer"`
}

var registry = map[int]ChainInfo{
	1:        {1, "Ethereum", "ETH", []string{"https://eth.drpc.org", "https://ethereum-rpc.publicnode.com"}, "https://etherscan.io"},
	8453:     {8453, "Base", "ETH", []string{"https://base.drpc.org", "https://mainnet.base.org"}, "https://basescan.org"},
	10:       {10, "Optimism", "ETH", []string{"https://mainnet.optimism.io"}, "https://optimistic.etherscan.io"},
	56:       {56, "BNB Chain", "BNB", []string{"https://bsc-dataseed.binance.org", "https://bsc.drpc.org"}, "https://bscscan.com"},
	137:      {137, "Polygon", "POL", []string{"https://polygon.drpc.org"}, "https://polygonscan.com"},
	59144:    {59144, "Linea", "ETH", []string{"https://linea.drpc.org"}, "https://lineascan.build"},
	360:      {360, "Shape", "ETH", []string{"https://mainnet.shape.network"}, "https://shapescan.xyz"},
	57073:    {57073, "Ink", "ETH", []string{"https://rpc-gel.inkonchain.com"}, "https://explorer.inkonchain.com"},
	2741:     {2741, "Abstract", "ETH", []string{"https://api.mainnet.abs.xyz"}, "https://abscan.org"},
	33139:    {33139, "ApeChain", "APE", []string{"https://rpc.apechain.com/http"}, "https://apescan.io"},
	11155111: {11155111, "Sepolia", "ETH", []string{"https://ethereum-sepolia-rpc.publicnode.com"}, "https://sepolia.etherscan.io"},
}

// rpcOverrides lets the operator point a chain at their own endpoint (e.g. a keyed
// RPC from .env) without persisting the secret to the DB. Set at startup.
var rpcOverrides = map[int][]string{}

// SetRPCs overrides the default RPC list for a chain (in-memory only).
func SetRPCs(chainID int, urls []string) {
	if len(urls) > 0 {
		rpcOverrides[chainID] = urls
	}
}

// Get returns the chain info or an error for unsupported chains.
func Get(chainID int) (ChainInfo, error) {
	c, ok := registry[chainID]
	if !ok {
		return ChainInfo{}, fmt.Errorf("unsupported chainId %d", chainID)
	}
	if ov, has := rpcOverrides[chainID]; has {
		c.RPCs = ov
	}
	return c, nil
}

// All returns every registered chain (order is not guaranteed).
func All() []ChainInfo {
	out := make([]ChainInfo, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	return out
}

// openSeaSlugs maps the OpenSea chain slug used in URLs/GraphQL to a chainId.
var openSeaSlugs = map[string]int{
	"ethereum": 1, "eth": 1, "base": 8453, "optimism": 10, "bsc": 56,
	"matic": 137, "polygon": 137, "linea": 59144, "shape": 360, "ink": 57073,
	"abstract": 2741, "apechain": 33139, "sepolia": 11155111,
}

// ChainIDFromSlug resolves an OpenSea slug to a chainId (0, false if unknown).
func ChainIDFromSlug(slug string) (int, bool) {
	id, ok := openSeaSlugs[strings.ToLower(slug)]
	return id, ok
}

// SlugFromChainID returns the OpenSea GraphQL slug for a chainId.
func SlugFromChainID(chainID int) (string, error) {
	switch chainID {
	case 1:
		return "ethereum", nil
	case 137:
		return "matic", nil
	}
	for slug, id := range openSeaSlugs {
		if id == chainID {
			return slug, nil
		}
	}
	return "", fmt.Errorf("no OpenSea slug for chainId %d", chainID)
}
