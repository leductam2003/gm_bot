// OpenSea GraphQL (gql.opensea.io) client for SeaDrop mint data:
//   - MintStages: all mint phases (public + allowlist) for a collection slug, with
//     start/end times and price. No auth (x-app-id only). Ported from zyper-mac
//     openseaVoucher.fetchEarliestStageStart (MintQuery).
//   - MintVoucher: a ready, server-signed mint transaction {to, data, value} for one
//     wallet — works for BOTH public and allowlist/FCFS stages (OpenSea picks the
//     active eligible stage). Ported from MintActionTimelineQuery (the os2 swap MINT
//     action). The bot just signs + sends; no calldata/proof building.
//
// Persisted-query hashes rotate when OpenSea ships a new web app; override via env
// (OPENSEA_MINTQUERY_HASH / OPENSEA_MINTACTION_HASH) — no recompile. MintVoucher
// self-heals a stale hash with a full-text POST fallback.
package opensea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const openseaGQL = "https://gql.opensea.io/graphql"

const zeroAddr = "0x0000000000000000000000000000000000000000"

func mintQueryHash() string {
	if v := os.Getenv("OPENSEA_MINTQUERY_HASH"); v != "" {
		return v
	}
	return "15d500b9dab94cd28b158ebc43ac446c3c71250178c22cdbdf152bc302154310"
}

func mintActionHash() string {
	if v := os.Getenv("OPENSEA_MINTACTION_HASH"); v != "" {
		return v
	}
	return "d8454b30426e34f3d5acec5f012d1bdedf31bb44199a83c9b6d05ff52fff8302"
}

func gqlHeaders(req *http.Request, op string) {
	req.Header.Set("accept", "application/graphql-response+json, application/json")
	req.Header.Set("referer", "https://opensea.io/")
	req.Header.Set("x-app-id", "os2-web")
	req.Header.Set("x-graphql-operation-name", op)
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
}

// Stage is one SeaDrop mint phase.
type Stage struct {
	Index     int    `json:"index"`
	Kind      string `json:"kind"` // "public" | "allowlist"
	StartUnix int64  `json:"startUnix"`
	EndUnix   int64  `json:"endUnix"`
	PriceWei  string `json:"priceWei"`
	PriceEth  string `json:"priceEth"`
}

// MintStages returns all mint phases for a collection slug, sorted by start time.
// Empty (no error) when the collection isn't an OpenSea SeaDrop with stages.
func (c *Client) MintStages(ctx context.Context, slug string) ([]Stage, error) {
	q := url.Values{}
	q.Set("operationName", "MintQuery")
	q.Set("variables", `{"slug":"`+slug+`"}`)
	q.Set("extensions", `{"persistedQuery":{"sha256Hash":"`+mintQueryHash()+`","version":1}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openseaGQL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	gqlHeaders(req, "MintQuery")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Data struct {
			CollectionBySlug struct {
				Drop struct {
					Stages []struct {
						StageIndex int    `json:"stageIndex"`
						StartTime  string `json:"startTime"`
						EndTime    string `json:"endTime"`
						Price      struct {
							Token struct {
								Unit json.Number `json:"unit"`
							} `json:"token"`
						} `json:"price"`
					} `json:"stages"`
				} `json:"drop"`
			} `json:"collectionBySlug"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var out []Stage
	for _, s := range r.Data.CollectionBySlug.Drop.Stages {
		kind := "allowlist"
		if s.StageIndex == 0 {
			kind = "public"
		}
		unit := s.Price.Token.Unit.String()
		out = append(out, Stage{
			Index:     s.StageIndex,
			Kind:      kind,
			StartUnix: parseISO(s.StartTime),
			EndUnix:   parseISO(s.EndTime),
			PriceWei:  decimalEthToWei(unit),
			PriceEth:  unit,
		})
	}
	// sort by start time ascending (allowlist usually first, then public)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartUnix < out[j-1].StartUnix; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Voucher is a ready-to-send mint transaction from OpenSea (signature embedded).
type Voucher struct {
	To       string
	Data     string
	ValueWei string
}

// MintVoucher fetches the ready mint tx for `minter` on a collection contract. Works
// for public + allowlist/FCFS (OpenSea returns the active eligible stage). chainSlug
// is the OpenSea chain slug (e.g. "ethereum", "base").
// httpClient returns the shared client, or a per-call client routed through proxyURL
// (for poll/snipe across many wallets without an IP ban).
func (c *Client) httpClient(proxyURL string) *http.Client {
	if proxyURL == "" {
		return c.hc
	}
	if pu, err := url.Parse(proxyURL); err == nil {
		return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	}
	return c.hc
}

func (c *Client) MintVoucher(ctx context.Context, minter, nftContract string, qty int, chainSlug, proxyURL string) (Voucher, error) {
	if qty < 1 {
		qty = 1
	}
	if chainSlug == "" {
		chainSlug = "ethereum"
	}
	variables := map[string]any{
		"address":      minter,
		"capabilities": map[string]any{"eip7702": false},
		"fromAssets":   []any{map[string]any{"asset": map[string]any{"chain": chainSlug, "contractAddress": zeroAddr}}},
		"toAssets":     []any{map[string]any{"asset": map[string]any{"chain": chainSlug, "contractAddress": nftContract, "tokenId": "0"}, "quantity": fmt.Sprint(qty)}},
	}
	varsJSON, _ := json.Marshal(variables)
	q := url.Values{}
	q.Set("operationName", "MintActionTimelineQuery")
	q.Set("variables", string(varsJSON))
	q.Set("extensions", `{"persistedQuery":{"sha256Hash":"`+mintActionHash()+`","version":1}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openseaGQL+"?"+q.Encode(), nil)
	if err != nil {
		return Voucher{}, err
	}
	gqlHeaders(req, "MintActionTimelineQuery")
	resp, err := c.httpClient(proxyURL).Do(req)
	if err != nil {
		return Voucher{}, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "PersistedQueryNotFound") {
		// hash rotated — resend as full-text POST (OpenSea accepts it).
		return c.mintVoucherFullText(ctx, minter, nftContract, qty, chainSlug, proxyURL)
	}
	return parseSwap(body)
}

const mintActionQuery = `query MintActionTimelineQuery($address: Address!, $fromAssets: [AssetQuantityInput!]!, $toAssets: [AssetQuantityInput!]!, $recipient: Address, $capabilities: WalletCapabilities) {
  swap(address: $address, fromAssets: $fromAssets, toAssets: $toAssets, recipient: $recipient, action: MINT, capabilities: $capabilities) {
    actions { __typename ... on TransactionAction { transactionSubmissionData { to data value } } }
    errors { __typename }
  }
}`

func (c *Client) mintVoucherFullText(ctx context.Context, minter, nftContract string, qty int, chainSlug, proxyURL string) (Voucher, error) {
	variables := map[string]any{
		"address":      minter,
		"recipient":    minter,
		"capabilities": map[string]any{"eip7702": false},
		"fromAssets":   []any{map[string]any{"asset": map[string]any{"chain": chainSlug, "contractAddress": zeroAddr}}},
		"toAssets":     []any{map[string]any{"asset": map[string]any{"chain": chainSlug, "contractAddress": nftContract, "tokenId": "0"}, "quantity": fmt.Sprint(qty)}},
	}
	payload, _ := json.Marshal(map[string]any{"operationName": "MintActionTimelineQuery", "query": mintActionQuery, "variables": variables})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openseaGQL, bytes.NewReader(payload))
	if err != nil {
		return Voucher{}, err
	}
	gqlHeaders(req, "MintActionTimelineQuery")
	req.Header.Set("content-type", "application/json")
	resp, err := c.httpClient(proxyURL).Do(req)
	if err != nil {
		return Voucher{}, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return parseSwap(body)
}

// parseSwap extracts {to,data,value} from a swap MINT response.
func parseSwap(body []byte) (Voucher, error) {
	var r struct {
		Data struct {
			Swap struct {
				Actions []struct {
					TransactionSubmissionData *struct {
						To    string      `json:"to"`
						Data  string      `json:"data"`
						Value json.Number `json:"value"`
					} `json:"transactionSubmissionData"`
				} `json:"actions"`
				Errors []struct {
					TypeName string `json:"__typename"`
				} `json:"errors"`
			} `json:"swap"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Voucher{}, fmt.Errorf("opensea mint: bad response")
	}
	if len(r.Errors) > 0 {
		return Voucher{}, fmt.Errorf("opensea mint gql: %s", r.Errors[0].Message)
	}
	if len(r.Data.Swap.Errors) > 0 {
		return Voucher{}, fmt.Errorf("opensea mint: %s", r.Data.Swap.Errors[0].TypeName)
	}
	for _, a := range r.Data.Swap.Actions {
		if a.TransactionSubmissionData != nil {
			t := a.TransactionSubmissionData
			val := t.Value.String()
			if val == "" {
				val = "0"
			}
			return Voucher{To: t.To, Data: t.Data, ValueWei: val}, nil
		}
	}
	return Voucher{}, fmt.Errorf("opensea mint: not eligible / stage not active")
}

// parseISO turns an RFC3339 timestamp into unix seconds (0 on failure/empty).
func parseISO(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// decimalEthToWei converts a decimal ETH string ("0.03") to a wei string exactly
// (no float rounding). Handles up to 18 fractional digits.
func decimalEthToWei(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "0"
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	if len(frac) > 18 {
		frac = frac[:18]
	}
	for len(frac) < 18 {
		frac += "0"
	}
	combined := strings.TrimLeft(intPart+frac, "0")
	if combined == "" {
		combined = "0"
	}
	// validate numeric
	if _, ok := new(big.Int).SetString(combined, 10); !ok {
		return "0"
	}
	if neg && combined != "0" {
		combined = "-" + combined
	}
	return combined
}
