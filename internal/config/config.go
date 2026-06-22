// Package config loads a local .env file (gitignored) into the process env and
// exposes typed accessors for the few secrets the bot uses: the default Ethereum
// RPC and the OpenSea API key. Secrets live only in .env + memory, never in the
// repo and never logged in full.
package config

import (
	"bufio"
	"os"
	"strings"
)

// Version is the running build, shown in the UI and used for update checks.
const Version = "1.3.0"

// LoadDotEnv reads KEY=VALUE lines from path and sets them in the environment if
// not already set. Missing file is not an error.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// EthRPC returns the operator-supplied Ethereum mainnet RPC, or "" if unset.
func EthRPC() string { return os.Getenv("ZYPER_ETH_RPC") }

// OpenSeaKeys returns all configured OpenSea API keys (newline/comma/space separated in
// OPENSEA_API_KEY). Multiple keys are rotated by the client to spread the rate limit.
func OpenSeaKeys() []string {
	var out []string
	for _, k := range strings.FieldsFunc(os.Getenv("OPENSEA_API_KEY"), func(r rune) bool {
		return r == '\n' || r == ',' || r == ' ' || r == '\t' || r == '\r'
	}) {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

// OpenSeaKey returns the first configured OpenSea API key ("" if none).
func OpenSeaKey() string {
	if ks := OpenSeaKeys(); len(ks) > 0 {
		return ks[0]
	}
	return ""
}

// EtherscanKey returns the Etherscan V2 (multichain) API key, used to fetch a
// verified contract's ABI. "" if unset (the user can paste the ABI instead).
func EtherscanKey() string { return os.Getenv("ETHERSCAN_API_KEY") }
