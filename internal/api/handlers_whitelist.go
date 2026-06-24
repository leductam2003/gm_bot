package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
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
	"zyperbot/internal/logger"
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

	threads := body.Threads
	if threads < 1 {
		threads = 5
	}
	if threads > 20 {
		threads = 20 // OpenSea throttles hard — cap concurrency (use proxies to go wider)
	}
	total := len(targets)
	// Return immediately and run the batch detached: a 200–500 wallet SIWE sweep far
	// outlasts the 30s HTTP timeout, which is what caused "context deadline exceeded".
	// Results stream over the WS ("whitelist" event); a final {done:true} marks completion.
	writeJSON(w, http.StatusOK, map[string]any{"runId": body.RunID, "slug": slug, "total": total})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		s.log.API(logger.INFO, "whitelist check started", map[string]any{"slug": slug, "wallets": total, "threads": threads})

		var mu sync.Mutex
		results := make(map[int64]wlResult, total)
		publish := func(res wlResult) {
			mu.Lock()
			results[res.WalletID] = res
			mu.Unlock()
			s.hub.Publish("whitelist", map[string]any{
				"runId": body.RunID, "slug": slug, "total": total,
				"walletId": res.WalletID, "address": res.Address, "label": res.Label,
				"ok": res.OK, "error": res.Error, "stages": res.Stages,
			})
		}
		// checkOne runs one wallet within its own time budget, retrying on error; it always
		// publishes a result so the slot frees and the run reaches 100% checked.
		checkOne := func(wl store.Wallet, px string) {
			res := wlResult{WalletID: wl.ID, Address: wl.Address, Label: wl.Label}
			defer func() { publish(res) }()
			key, _, kerr := s.walletKey(wl.ID)
			if kerr != nil {
				res.Error = "key: " + kerr.Error()
				return
			}
			defer wipeECDSA(key)
			addr := common.HexToAddress(wl.Address)
			wctx, wcancel := context.WithTimeout(ctx, wlWalletBudget)
			defer wcancel()
			for attempt := 0; attempt < wlMaxAttempts; attempt++ {
				if wctx.Err() != nil {
					if res.Error == "" {
						res.Error = "timed out"
					}
					return
				}
				stages, cerr := s.checkWalletWhitelist(wctx, key, addr, slug, px)
				if cerr == nil {
					res.OK, res.Stages, res.Error = true, stages, ""
					return
				}
				res.Error = cerr.Error()
				if !wlSleep(wctx, wlBackoff(attempt)) {
					return
				}
			}
		}
		runRound := func(wallets []store.Wallet, conc int) {
			if conc < 1 {
				conc = 1
			}
			sem := make(chan struct{}, conc)
			var wg sync.WaitGroup
			for i, wl := range wallets {
				if ctx.Err() != nil {
					break
				}
				wg.Add(1)
				go func(i int, wl store.Wallet) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					px := ""
					if n := len(proxies); n > 0 {
						px = proxies[i%n]
					}
					checkOne(wl, px)
				}(i, wl)
			}
			wg.Wait()
		}

		runRound(targets, threads) // round 1: everyone at the configured concurrency
		// Retry rounds: re-check only the wallets without a result yet, at LOW concurrency so
		// they stop competing for OpenSea's per-IP budget — that's what gets the throttled
		// stragglers to a real result instead of staying "unchecked".
		for round := 0; round < wlRetryRounds && ctx.Err() == nil; round++ {
			mu.Lock()
			var fails []store.Wallet
			for _, wl := range targets {
				r, ok := results[wl.ID]
				if ok && (r.OK || strings.HasPrefix(r.Error, "key:")) {
					continue // already done, or a permanent key-decrypt failure — don't retry
				}
				fails = append(fails, wl)
			}
			mu.Unlock()
			if len(fails) == 0 {
				break
			}
			s.log.API(logger.INFO, "whitelist retry round", map[string]any{"round": round + 1, "remaining": len(fails)})
			wlSleep(ctx, 3*time.Second) // cool-down so the per-IP budget recovers
			runRound(fails, wlRetryConc)
		}

		// If the parent context was cancelled mid-round, some wallets may never have run.
		// Publish a result for each so the client reaches total/total instead of hanging.
		mu.Lock()
		var missing []wlResult
		for _, wl := range targets {
			if _, done := results[wl.ID]; !done {
				missing = append(missing, wlResult{WalletID: wl.ID, Address: wl.Address, Label: wl.Label, Error: "cancelled"})
			}
		}
		mu.Unlock()
		for _, m := range missing {
			publish(m)
		}

		mu.Lock()
		ok := 0
		for _, r := range results {
			if r.OK {
				ok++
			}
		}
		mu.Unlock()
		lvl := logger.INFO
		if ok < total {
			lvl = logger.WARN
		}
		s.log.API(lvl, "whitelist check complete", map[string]any{"slug": slug, "eligible_checked": ok, "failed": total - ok, "wallets": total})
		s.hub.Publish("whitelist", map[string]any{"runId": body.RunID, "slug": slug, "total": total, "done": true, "eligible": ok})
	}()
}

const (
	wlMaxAttempts  = 6                // retries per wallet within its budget (bounded by wlWalletBudget)
	wlWalletBudget = 75 * time.Second // hard cap on one wallet's total check time, so the run can't stall
	wlRetryRounds  = 3                // extra passes over the still-unfinished wallets
	wlRetryConc    = 2                // low concurrency for retries → less per-IP throttling, more successes
)

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
	return &http.Client{Timeout: 15 * time.Second, Jar: jar, Transport: tr}
}

func wlBrowserHeaders(req *http.Request) {
	req.Header.Set("origin", "https://opensea.io")
	req.Header.Set("referer", "https://opensea.io/")
	req.Header.Set("user-agent", wlUA)
	req.Header.Set("x-app-id", "os2-web")
	req.Header.Set("accept", "application/json")
}

// --- SIWE session cache: reuse a wallet's access_token across checks so we don't re-sign
// every time. Tokens are short-lived JWTs; we honor their exp and re-auth on rejection.

type wlSession struct {
	token string
	exp   time.Time
}

var wlTokens sync.Map // lowercased address -> wlSession (in-memory hot path over the DB)

// cachedToken returns a still-valid session token for the wallet: in-memory first, then the
// DB (so a token survives app restarts and we don't re-sign every run).
func (s *Server) cachedToken(addr string) string {
	if v, ok := wlTokens.Load(addr); ok {
		ws := v.(wlSession)
		if time.Now().Before(ws.exp.Add(-2 * time.Minute)) { // small safety margin
			return ws.token
		}
		wlTokens.Delete(addr)
	}
	if tok, exp, ok := s.st.GetWLSession(addr); ok {
		e := time.Unix(exp, 0)
		if time.Now().Before(e.Add(-2 * time.Minute)) {
			wlTokens.Store(addr, wlSession{token: tok, exp: e}) // warm the memory cache
			return tok
		}
		s.st.DeleteWLSession(addr) // expired
	}
	return ""
}

// cacheToken persists a fresh session token to both the memory cache and the DB.
func (s *Server) cacheToken(addr, token string) {
	if token == "" {
		return
	}
	exp := tokenExpiry(token)
	wlTokens.Store(addr, wlSession{token: token, exp: exp})
	_ = s.st.SaveWLSession(addr, token, exp.Unix())
}

// clearWLToken drops a rejected/expired token from memory and the DB.
func (s *Server) clearWLToken(addr string) {
	wlTokens.Delete(addr)
	s.st.DeleteWLSession(addr)
}

// tokenExpiry reads the `exp` claim from a JWT access_token; falls back to 20 minutes.
func tokenExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) == 3 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var c struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &c) == nil && c.Exp > 0 {
				return time.Unix(c.Exp, 0)
			}
		}
	}
	return time.Now().Add(20 * time.Minute)
}

// checkWalletWhitelist queries drop eligibility for one wallet. It reuses a cached SIWE
// session token when available (no signing); only on first check or token rejection does it
// run the full SIWE sign-in (nonce → sign → verify).
func (s *Server) checkWalletWhitelist(ctx context.Context, key *ecdsa.PrivateKey, addr common.Address, slug, proxyURL string) ([]wlStage, error) {
	hc := wlHTTPClient(proxyURL)
	lc := strings.ToLower(addr.Hex())

	if tok := s.cachedToken(lc); tok != "" {
		if stages, err := dropEligibility(ctx, hc, slug, addr, tok); err == nil {
			return stages, nil
		}
		s.clearWLToken(lc) // cached token rejected/expired — fall back to a fresh sign-in
	}

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
	s.cacheToken(lc, token)
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
	for attempt := 0; attempt < 4; attempt++ {
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
