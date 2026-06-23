package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"zyperbot/internal/chains"
	"zyperbot/internal/store"
)

const siweStatement = "Click to sign in and accept the OpenSea Terms of Service (https://opensea.io/tos) and Privacy Policy (https://opensea.io/privacy)."
const wlUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

const dropEligibilityQuery = `query DropEligibilityQuery($collectionSlug: String!, $address: Address!) {
  dropBySlug(slug: $collectionSlug) {
    __typename
    ... on Erc721SeaDropV1 { minterQuantityMinted(minter: $address) }
    stages { stageType stageIndex isEligible maxTotalMintableByWallet eligibleMaxTotalMintableByWallet }
  }
}`

type wlStage struct {
	StageType   string `json:"stageType"`
	StageIndex  int    `json:"stageIndex"`
	IsEligible  bool   `json:"isEligible"`
	Max         int    `json:"maxTotalMintableByWallet"`
	EligibleMax int    `json:"eligibleMaxTotalMintableByWallet"`
}

type wlResult struct {
	WalletID int64     `json:"walletId"`
	Address  string    `json:"address"`
	Label    string    `json:"label"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Stages   []wlStage `json:"stages,omitempty"`
}

// POST /api/whitelist/check {link, walletIds?, group?, proxyGroup?, chainId?}
// For each wallet: SIWE sign-in to OpenSea (nonce → sign → verify) then query per-wallet
// drop eligibility across every mint stage. Concurrency is bounded; route through a
// proxy group to avoid IP throttling on many wallets.
func (s *Server) handleWhitelistCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link       string  `json:"link"`
		WalletIDs  []int64 `json:"walletIds"`
		Group      string  `json:"group"`
		ProxyGroup string  `json:"proxyGroup"`
		ChainID    int     `json:"chainId"`
		Threads    int     `json:"threads"`
		RunID      string  `json:"runId"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	slug, err := s.resolveCollectionSlug(r.Context(), body.Link, body.ChainID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not resolve collection: "+err.Error())
		return
	}
	rows, err := s.st.ListWallets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := map[int64]bool{}
	for _, id := range body.WalletIDs {
		want[id] = true
	}
	var proxies []string
	if body.ProxyGroup != "" {
		if ps, _ := s.st.ListProxiesByGroup(body.ProxyGroup); ps != nil {
			for _, p := range ps {
				proxies = append(proxies, p.URL)
			}
		}
	}
	var targets []store.Wallet
	for _, wl := range rows {
		if wl.Network != "evm" {
			continue
		}
		if len(want) > 0 && !want[wl.ID] {
			continue
		}
		if body.Group != "" && wl.GroupName != body.Group {
			continue
		}
		targets = append(targets, wl)
	}

	out := make([]wlResult, len(targets))
	threads := body.Threads
	if threads < 1 {
		threads = 5
	}
	if threads > 20 {
		threads = 20 // OpenSea throttles hard — cap concurrency (use proxies to go wider)
	}
	sem := make(chan struct{}, threads)
	var wg sync.WaitGroup
	for i, wl := range targets {
		wg.Add(1)
		go func(i int, wl store.Wallet) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := wlResult{WalletID: wl.ID, Address: wl.Address, Label: wl.Label}
			// Publish each wallet's result over the WS the moment it finishes so the UI
			// fills in row-by-row instead of waiting for the whole batch.
			defer func() {
				out[i] = res
				s.hub.Publish("whitelist", map[string]any{
					"runId": body.RunID, "slug": slug, "total": len(targets),
					"walletId": res.WalletID, "address": res.Address, "label": res.Label,
					"ok": res.OK, "error": res.Error, "stages": res.Stages,
				})
			}()
			key, _, kerr := s.walletKey(wl.ID)
			if kerr != nil {
				res.Error = "key: " + kerr.Error()
				return
			}
			px := ""
			if n := len(proxies); n > 0 {
				px = proxies[i%n]
			}
			stages, cerr := checkWalletWhitelist(r.Context(), key, common.HexToAddress(wl.Address), slug, px)
			wipeECDSA(key)
			if cerr != nil {
				res.Error = cerr.Error()
				return
			}
			res.OK = true
			res.Stages = stages
		}(i, wl)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "wallets": out})
}

// resolveCollectionSlug turns an OpenSea collection link / bare slug / contract address
// into a collection slug.
func (s *Server) resolveCollectionSlug(ctx context.Context, link string, chainID int) (string, error) {
	v := strings.TrimSpace(link)
	if v == "" {
		return "", fmt.Errorf("empty link")
	}
	if i := strings.Index(strings.ToLower(v), "/collection/"); i >= 0 {
		rest := strings.TrimRight(v[i+len("/collection/"):], "/")
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		if rest != "" {
			return rest, nil
		}
	}
	if common.IsHexAddress(v) {
		cs := chainID
		if cs == 0 {
			cs = 1
		}
		slugChain, _ := chains.SlugFromChainID(cs)
		return s.osc.ContractSlug(ctx, slugChain, common.HexToAddress(v).Hex())
	}
	if !strings.Contains(v, "/") {
		return v, nil // bare slug
	}
	return "", fmt.Errorf("paste an OpenSea collection link or contract address")
}

func wlHTTPClient(proxyURL string) *http.Client {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{}
	if proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{Timeout: 25 * time.Second, Jar: jar, Transport: tr}
}

func wlBrowserHeaders(req *http.Request) {
	req.Header.Set("origin", "https://opensea.io")
	req.Header.Set("referer", "https://opensea.io/")
	req.Header.Set("user-agent", wlUA)
	req.Header.Set("x-app-id", "os2-web")
	req.Header.Set("accept", "application/json")
}

// checkWalletWhitelist runs the full SIWE sign-in + eligibility query for one wallet.
func checkWalletWhitelist(ctx context.Context, key *ecdsa.PrivateKey, addr common.Address, slug, proxyURL string) ([]wlStage, error) {
	hc := wlHTTPClient(proxyURL)
	nonce, err := siweNonce(ctx, hc)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	issuedAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	siwe, msgObj := buildSIWE(addr, slug, nonce, issuedAt)
	sig, err := personalSign(key, siwe)
	if err != nil {
		return nil, err
	}
	if err := siweVerify(ctx, hc, msgObj, sig); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	token := ""
	if u, e := url.Parse("https://opensea.io/"); e == nil {
		for _, ck := range hc.Jar.Cookies(u) {
			if ck.Name == "access_token" {
				token = ck.Value
			}
		}
	}
	return dropEligibility(ctx, hc, slug, addr, token)
}

// buildSIWE returns the EIP-4361 message string to sign + the message object OpenSea's
// verify endpoint expects (server reconstructs the same string to check the signature).
func buildSIWE(addr common.Address, slug, nonce, issuedAt string) (string, map[string]any) {
	a := addr.Hex()
	uri := "https://opensea.io/collection/" + slug + "/overview"
	siwe := "opensea.io wants you to sign in with your Ethereum account:\n" + a +
		"\n\n" + siweStatement +
		"\n\nURI: " + uri + "\nVersion: 1\nChain ID: 1\nNonce: " + nonce + "\nIssued At: " + issuedAt
	obj := map[string]any{
		"accountType": "Ethereum", "address": a, "chainId": "1", "domain": "opensea.io",
		"issuedAt": issuedAt, "nonce": nonce, "statement": siweStatement, "uri": uri, "version": "1",
	}
	return siwe, obj
}

func personalSign(key *ecdsa.PrivateKey, msg string) (string, error) {
	sig, err := gethcrypto.Sign(accounts.TextHash([]byte(msg)), key)
	if err != nil {
		return "", err
	}
	sig[64] += 27 // V 0/1 -> 27/28
	return "0x" + hex.EncodeToString(sig), nil
}

func siweNonce(ctx context.Context, hc *http.Client) (string, error) {
	b, status, err := wlDo(ctx, hc, func() (*http.Request, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://opensea.io/__api/auth/siwe/nonce", strings.NewReader("{}"))
		wlBrowserHeaders(req)
		req.Header.Set("content-type", "application/json")
		return req, nil
	})
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("%d: %s", status, snipB(b))
	}
	var j struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(b, &j) == nil && j.Nonce != "" {
		return j.Nonce, nil
	}
	var sv string
	if json.Unmarshal(b, &sv) == nil && sv != "" {
		return sv, nil
	}
	return strings.Trim(strings.TrimSpace(string(b)), `"`), nil
}

func siweVerify(ctx context.Context, hc *http.Client, msgObj map[string]any, sig string) error {
	payload, _ := json.Marshal(map[string]any{"chainArch": "EVM", "connectorId": "io.metamask", "message": msgObj, "signature": sig})
	b, status, err := wlDo(ctx, hc, func() (*http.Request, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://opensea.io/__api/auth/siwe/verify", bytes.NewReader(payload))
		wlBrowserHeaders(req)
		req.Header.Set("content-type", "application/json")
		return req, nil
	})
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("%d: %s", status, snipB(b))
	}
	return nil
}

// wlDo runs build() and retries on 429 / 5xx with backoff (honoring Retry-After), since
// OpenSea throttles hard when many wallets check in. Non-retryable responses (incl. other
// 4xx) are returned with their status; only the transport/exhaustion case sets err.
func wlDo(ctx context.Context, hc *http.Client, build func() (*http.Request, error)) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		req, err := build()
		if err != nil {
			return nil, 0, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			if !wlSleep(ctx, wlBackoff(attempt)) {
				return nil, 0, ctx.Err()
			}
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		wait := wlRetryAfter(resp)
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%d: %s", resp.StatusCode, snipB(b))
			if wait <= 0 {
				wait = wlBackoff(attempt)
			}
			if !wlSleep(ctx, wait) {
				return nil, 0, ctx.Err()
			}
			continue
		}
		return b, resp.StatusCode, nil
	}
	return nil, 0, lastErr
}

func wlRetryAfter(resp *http.Response) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			if secs > 10 {
				secs = 10
			}
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

func wlBackoff(attempt int) time.Duration {
	d := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func wlSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func dropEligibility(ctx context.Context, hc *http.Client, slug string, addr common.Address, token string) ([]wlStage, error) {
	payload, _ := json.Marshal(map[string]any{
		"operationName": "DropEligibilityQuery",
		"query":         dropEligibilityQuery,
		"variables":     map[string]any{"address": strings.ToLower(addr.Hex()), "collectionSlug": slug},
	})
	// connected-account-server-hint tells OpenSea which wallet is "active" — without it
	// the query returns isEligible:null / ACTIVE_ADDRESS_NOT_PROVIDED (personalized
	// eligibility needs both the access_token AND the active-address hint).
	cookie := "connected-account-server-hint=" + strings.ToLower(addr.Hex()) + "; auth_access_hint=true"
	if token != "" {
		cookie = "access_token=" + token + "; " + cookie
	}
	b, status, err := wlDo(ctx, hc, func() (*http.Request, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://gql.opensea.io/graphql", bytes.NewReader(payload))
		req.Header.Set("origin", "https://opensea.io")
		req.Header.Set("referer", "https://opensea.io/")
		req.Header.Set("user-agent", wlUA)
		req.Header.Set("x-app-id", "os2-web")
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "application/graphql-response+json, application/json")
		req.Header.Set("cookie", cookie)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("eligibility %d: %s", status, snipB(b))
	}
	var r struct {
		Data struct {
			DropBySlug struct {
				Stages []wlStage `json:"stages"`
			} `json:"dropBySlug"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("gql: %s", r.Errors[0].Message)
	}
	return r.Data.DropBySlug.Stages, nil
}

func snipB(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
