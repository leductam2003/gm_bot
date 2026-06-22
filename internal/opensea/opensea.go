// Package opensea is a thin client for the OpenSea API v2 (used for the NFT manager:
// fetching a wallet's items in a collection, their images, and active listings).
// The API key comes from config (OPENSEA_API_KEY in .env) and is never logged.
package opensea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"zyperbot/internal/config"
)

const base = "https://api.opensea.io/api/v2"

type Client struct {
	hc     *http.Client
	keyIdx atomic.Uint64 // round-robin cursor across the configured OpenSea keys
}

func New() *Client { return &Client{hc: &http.Client{Timeout: 15 * time.Second}} }

func (c *Client) HasKey() bool { return config.OpenSeaKey() != "" }

func (c *Client) get(ctx context.Context, path string) ([]byte, int, error) {
	return c.doRotating(ctx, http.MethodGet, path, nil, "")
}

func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, int, error) {
	b, _ := json.Marshal(payload)
	return c.doRotating(ctx, http.MethodPost, path, b, "application/json")
}

// doRotating sends a request, rotating across all configured OpenSea API keys: it
// round-robins the starting key (to spread load) and retries with the next key on a
// rate-limit/auth response (429/401/403), so one throttled or bad key falls back to
// another. Other 4xx/5xx are returned as-is.
func (c *Client) doRotating(ctx context.Context, method, path string, body []byte, ctype string) ([]byte, int, error) {
	keys := config.OpenSeaKeys()
	attempts := len(keys)
	if attempts == 0 {
		attempts = 1
	}
	var lastBody []byte
	var lastStatus int
	var lastErr error
	for i := 0; i < attempts; i++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("accept", "application/json")
		if ctype != "" {
			req.Header.Set("content-type", ctype)
		}
		if len(keys) > 0 {
			req.Header.Set("x-api-key", keys[int(c.keyIdx.Add(1)-1)%len(keys)])
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode == 401 || resp.StatusCode == 403 {
			lastBody, lastStatus, lastErr = rb, resp.StatusCode, fmt.Errorf("opensea %d: %s", resp.StatusCode, snip(rb))
			continue // throttled/bad key — rotate to the next
		}
		if resp.StatusCode >= 400 {
			return rb, resp.StatusCode, fmt.Errorf("opensea %d: %s", resp.StatusCode, snip(rb))
		}
		return rb, resp.StatusCode, nil
	}
	return lastBody, lastStatus, lastErr
}

func snip(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// Fee is a collection fee (basis points).
type Fee struct {
	Recipient string
	Bps       int64
}

// CollectionFees returns the enforced fees for a collection (OpenSea + creator).
func (c *Client) CollectionFees(ctx context.Context, slug string) ([]Fee, error) {
	body, _, err := c.get(ctx, "/collections/"+slug)
	if err != nil {
		return nil, err
	}
	var r struct {
		Fees []struct {
			Fee       float64 `json:"fee"`
			Recipient string  `json:"recipient"`
		} `json:"fees"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var out []Fee
	for _, f := range r.Fees {
		bps := int64(math.Round(f.Fee * 100)) // 2.5% -> 250 bps
		if bps > 0 && f.Recipient != "" {
			out = append(out, Fee{Recipient: f.Recipient, Bps: bps})
		}
	}
	return out, nil
}

// PostListing submits a signed Seaport listing to OpenSea.
func (c *Client) PostListing(ctx context.Context, chain string, listing any) error {
	_, _, err := c.post(ctx, "/orders/"+chain+"/seaport/listings", listing)
	return err
}

// NFT is one item owned by a wallet.
type NFT struct {
	TokenID  string `json:"tokenId"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Contract string `json:"contract"`
}

// ContractSlug resolves a contract address to its OpenSea collection slug.
func (c *Client) ContractSlug(ctx context.Context, chain, contract string) (string, error) {
	body, _, err := c.get(ctx, "/chain/"+chain+"/contract/"+contract)
	if err != nil {
		return "", err
	}
	var r struct {
		Collection string `json:"collection"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	return r.Collection, nil
}

// CollectionInfo is the resolved primary contract for a collection slug.
type CollectionInfo struct {
	Name     string
	Contract string
	Chain    string
}

// Collection resolves a collection slug to its primary contract + chain.
func (c *Client) Collection(ctx context.Context, slug string) (CollectionInfo, error) {
	body, _, err := c.get(ctx, "/collections/"+slug)
	if err != nil {
		return CollectionInfo{}, err
	}
	var r struct {
		Name      string `json:"name"`
		Contracts []struct {
			Address string `json:"address"`
			Chain   string `json:"chain"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return CollectionInfo{}, err
	}
	if len(r.Contracts) == 0 {
		return CollectionInfo{}, fmt.Errorf("collection %q has no contract", slug)
	}
	return CollectionInfo{Name: r.Name, Contract: r.Contracts[0].Address, Chain: r.Contracts[0].Chain}, nil
}

// AccountNFTs returns a wallet's NFTs (optionally filtered to a collection slug),
// paginating up to `limit` items.
func (c *Client) AccountNFTs(ctx context.Context, chain, address, collection string, limit int) ([]NFT, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []NFT
	next := ""
	for page := 0; page < 8 && len(out) < limit; page++ {
		q := url.Values{}
		q.Set("limit", "50")
		if collection != "" {
			q.Set("collection", collection)
		}
		if next != "" {
			q.Set("next", next)
		}
		body, _, err := c.get(ctx, fmt.Sprintf("/chain/%s/account/%s/nfts?%s", chain, address, q.Encode()))
		if err != nil {
			return out, err
		}
		var r struct {
			Nfts []struct {
				Identifier      string `json:"identifier"`
				Name            string `json:"name"`
				ImageURL        string `json:"image_url"`
				DisplayImageURL string `json:"display_image_url"`
				Contract        string `json:"contract"`
			} `json:"nfts"`
			Next string `json:"next"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return out, err
		}
		for _, n := range r.Nfts {
			img := n.DisplayImageURL
			if img == "" {
				img = n.ImageURL
			}
			name := n.Name
			if name == "" {
				name = "#" + n.Identifier
			}
			out = append(out, NFT{TokenID: n.Identifier, Name: name, Image: img, Contract: n.Contract})
		}
		next = r.Next
		if next == "" {
			break
		}
	}
	return out, nil
}

// MakerListedTokenIDs returns the set of token IDs the maker currently has listed
// for a collection (best-effort; an error returns an empty set).
func (c *Client) MakerListedTokenIDs(ctx context.Context, chain, slug, maker, contract string) map[string]bool {
	set := map[string]bool{}
	if slug == "" {
		return set
	}
	// /api/v2/listings/collection/{slug}/all returns active listings for the collection.
	next := ""
	lc := strings.ToLower(contract)
	lm := strings.ToLower(maker)
	for page := 0; page < 6; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		if next != "" {
			q.Set("next", next)
		}
		body, _, err := c.get(ctx, fmt.Sprintf("/listings/collection/%s/all?%s", slug, q.Encode()))
		if err != nil {
			return set
		}
		var r struct {
			Listings []struct {
				ProtocolData struct {
					Parameters struct {
						Offerer string `json:"offerer"`
						Offer   []struct {
							Token                string `json:"token"`
							IdentifierOrCriteria string `json:"identifierOrCriteria"`
						} `json:"offer"`
					} `json:"parameters"`
				} `json:"protocol_data"`
			} `json:"listings"`
			Next string `json:"next"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return set
		}
		for _, l := range r.Listings {
			if strings.ToLower(l.ProtocolData.Parameters.Offerer) != lm {
				continue
			}
			for _, o := range l.ProtocolData.Parameters.Offer {
				if strings.ToLower(o.Token) == lc {
					set[o.IdentifierOrCriteria] = true
				}
			}
		}
		next = r.Next
		if next == "" {
			break
		}
	}
	return set
}
