package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"zyperbot/internal/chains"
	"zyperbot/internal/engine"
	"zyperbot/internal/evm"
	"zyperbot/internal/opensea"
	"zyperbot/internal/logger"
)

// walletKey decrypts a wallet's private key via the vault (caller must wipe it).
func (s *Server) walletKey(id int64) (*ecdsa.PrivateKey, common.Address, error) {
	wl, err := s.st.GetWallet(id)
	if err != nil {
		return nil, common.Address{}, err
	}
	pk, err := s.vault.Open(wl.EncPrivKey)
	if err != nil {
		return nil, common.Address{}, err
	}
	trimmed := bytes.TrimPrefix(bytes.TrimSpace(pk), []byte("0x"))
	raw := make([]byte, hex.DecodedLen(len(trimmed)))
	_, hxerr := hex.Decode(raw, trimmed)
	var key *ecdsa.PrivateKey
	if hxerr == nil {
		key, err = gethcrypto.ToECDSA(raw)
	} else {
		err = hxerr
	}
	for i := range pk {
		pk[i] = 0
	}
	for i := range raw {
		raw[i] = 0
	}
	if err != nil {
		return nil, common.Address{}, err
	}
	return key, gethcrypto.PubkeyToAddress(key.PublicKey), nil
}

func wipeECDSA(key *ecdsa.PrivateKey) {
	if key != nil && key.D != nil {
		bits := key.D.Bits()
		for i := range bits {
			bits[i] = 0
		}
		key.D.SetInt64(0)
	}
}

func randSalt() *big.Int {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return new(big.Int).SetBytes(b)
}

func autoGas() evm.GasParams { return evm.GasParams{Mode: evm.GasAuto} }

// clientForChain dials the first configured (or registry-default) RPC for a chain.
func (s *Server) clientForChain(r *http.Request, chainID int) (*ethclient.Client, error) {
	url := ""
	if es, _ := s.st.ListRPCByChain(chainID); len(es) > 0 {
		url = es[0].URL
	} else if c, err := chains.Get(chainID); err == nil && len(c.RPCs) > 0 {
		url = c.RPCs[0]
	}
	if url == "" {
		return nil, errors.New("no rpc for chain")
	}
	return s.pool.Dial(r.Context(), url)
}

type holding struct {
	Address string `json:"address"`
	Label   string `json:"label"`
	Balance string `json:"balance"`
	Err     string `json:"err,omitempty"`
}

// POST /api/nft/holdings {chainId, contractAddress, walletIds?, group?}
// Returns each selected wallet's balanceOf in the collection.
func (s *Server) handleNftHoldings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID         int     `json:"chainId"`
		ContractAddress string  `json:"contractAddress"`
		WalletIDs       []int64 `json:"walletIds"`
		Group           string  `json:"group"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !common.IsHexAddress(body.ContractAddress) {
		writeErr(w, http.StatusBadRequest, "invalid contract address")
		return
	}
	client, err := s.clientForChain(r, body.ChainID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	contract := common.HexToAddress(body.ContractAddress)

	ws, err := s.st.ListWallets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := map[int64]bool{}
	for _, id := range body.WalletIDs {
		want[id] = true
	}
	type item struct {
		addr  common.Address
		label string
	}
	var items []item
	for _, wl := range ws {
		if wl.Network != "evm" {
			continue
		}
		if body.Group != "" && wl.GroupName != body.Group {
			continue
		}
		if len(want) > 0 && !want[wl.ID] {
			continue
		}
		items = append(items, item{common.HexToAddress(wl.Address), wl.Label})
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"holdings": []holding{}, "total": "0"})
		return
	}

	out := make([]holding, len(items))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			bal, berr := evm.BalanceOf(ctx, client, contract, it.addr)
			h := holding{Address: it.addr.Hex(), Label: it.label}
			if berr != nil {
				h.Err = berr.Error()
			} else {
				h.Balance = bal.String()
			}
			out[i] = h
		}(i, it)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"holdings": out, "name": evm.CollectionName(r.Context(), client, contract)})
}

// POST /api/nft/resolve-link {link, chainId?} — turn an OpenSea URL/slug/address into
// a contract + chain (+ SeaDrop drop info) to auto-fill the Create Task form.
func (s *Server) handleNftResolveLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link    string `json:"link"`
		ChainID int    `json:"chainId"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	v := strings.TrimSpace(body.Link)
	if v == "" {
		writeErr(w, http.StatusBadRequest, "empty link")
		return
	}
	chainID := body.ChainID
	if chainID == 0 {
		chainID = 1
	}
	contract, slug := "", ""
	switch {
	case common.IsHexAddress(v):
		contract = v
	case strings.Contains(strings.ToLower(v), "opensea.io"):
		u := v
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		parts := strings.Split(strings.Trim(u, "/"), "/")
		for i, p := range parts {
			lp := strings.ToLower(p)
			if lp == "collection" && i+1 < len(parts) {
				slug = parts[i+1]
				break
			}
			if (lp == "assets" || lp == "item") && i+2 < len(parts) {
				if id, ok := chains.ChainIDFromSlug(parts[i+1]); ok {
					chainID = id
				}
				if common.IsHexAddress(parts[i+2]) {
					contract = parts[i+2]
				}
				break
			}
		}
	default:
		slug = v // bare collection slug
	}

	name := ""
	if contract == "" && slug != "" {
		if !s.osc.HasKey() {
			writeErr(w, http.StatusBadRequest, "OpenSea API key not set — needed to resolve a collection slug")
			return
		}
		info, err := s.osc.Collection(r.Context(), slug)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "resolve: "+err.Error())
			return
		}
		contract, name = info.Contract, info.Name
		if id, ok := chains.ChainIDFromSlug(info.Chain); ok {
			chainID = id
		}
	}
	if !common.IsHexAddress(contract) {
		writeErr(w, http.StatusBadRequest, "could not resolve a contract from that link")
		return
	}

	out := map[string]any{"contractAddress": common.HexToAddress(contract).Hex(), "chainId": chainID, "slug": slug, "name": name, "seadrop": false, "maxPerWallet": 1}
	if client, err := s.clientForChain(r, chainID); err == nil {
		cAddr := common.HexToAddress(contract)
		if name == "" {
			out["name"] = evm.CollectionName(r.Context(), client, cAddr)
		}
		if evm.IsSeaDropMintable(r.Context(), client, cAddr) {
			if res, e := evm.ResolveSeaDrop(r.Context(), client, cAddr, 1, common.Address{}); e == nil {
				out["seadrop"] = true
				out["priceWei"] = res.Drop.MintPrice.String()
				out["maxPerWallet"] = res.Drop.MaxTotalMintableByWallet
			}
		}
	}

	// All mint phases (public + allowlist) from OpenSea GraphQL. Needs a slug; if the
	// link was a raw address, resolve contract -> slug first.
	phaseSlug := slug
	if phaseSlug == "" {
		if cs, ok := chains.SlugFromChainID(chainID); ok == nil {
			if sl, e := s.osc.ContractSlug(r.Context(), cs, common.HexToAddress(contract).Hex()); e == nil {
				phaseSlug = sl
			}
		}
	}
	if phaseSlug != "" {
		if phases, e := s.osc.MintStages(r.Context(), phaseSlug); e == nil && len(phases) > 0 {
			out["phases"] = phases
			out["seadrop"] = true
			out["slug"] = phaseSlug
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type nftItem struct {
	WalletID int64  `json:"walletId"`
	Owner    string `json:"owner"`
	TokenID  string `json:"tokenId"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Listed   bool   `json:"listed"`
}

// POST /api/nft/items {chainId, contractAddress, walletIds?, group?}
// Returns the selected wallets' NFTs in a collection with images + listing status.
type selWallet struct {
	id   int64
	addr string
}

type nftStreamReq struct {
	runID     string
	total     int
	chainSlug string
	contract  string
	threads   int
	selected  []selWallet
	proxyURLs []string
}

// nftStreamResult is one streamed wallet's NFTs (WS "nft" event); Done=true ends the run.
type nftStreamResult struct {
	RunID    string    `json:"runId"`
	Total    int       `json:"total"`
	Index    int       `json:"index"`
	WalletID int64     `json:"walletId,omitempty"`
	Owner    string    `json:"owner,omitempty"`
	Items    []nftItem `json:"items,omitempty"`
	Slug     string    `json:"slug,omitempty"`
	Done     bool      `json:"done,omitempty"`
	Failed   int       `json:"failed,omitempty"`
}

func (s *Server) handleNftItems(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID         int     `json:"chainId"`
		ContractAddress string  `json:"contractAddress"`
		WalletIDs       []int64 `json:"walletIds"`
		Group           string  `json:"group"`
		Threads         int     `json:"threads"`
		ProxyGroup      string  `json:"proxyGroup"`
		RunID           string  `json:"runId"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !s.osc.HasKey() {
		writeErr(w, http.StatusBadRequest, "OpenSea API key not set — add OPENSEA_API_KEY in .env")
		return
	}
	if !common.IsHexAddress(body.ContractAddress) {
		writeErr(w, http.StatusBadRequest, "invalid contract address")
		return
	}
	chainSlug, err := chains.SlugFromChainID(body.ChainID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "chain not supported by OpenSea")
		return
	}
	ws, err := s.st.ListWallets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := map[int64]bool{}
	for _, id := range body.WalletIDs {
		want[id] = true
	}
	var selected []selWallet
	for _, wl := range ws {
		if wl.Network != "evm" {
			continue
		}
		if body.Group != "" && wl.GroupName != body.Group {
			continue
		}
		if len(want) > 0 && !want[wl.ID] {
			continue
		}
		selected = append(selected, selWallet{wl.ID, wl.Address})
	}
	if len(selected) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"runId": body.RunID, "total": 0})
		return
	}

	var proxyURLs []string
	if body.ProxyGroup != "" {
		if ps, _ := s.st.ListProxiesByGroup(body.ProxyGroup); ps != nil {
			for _, p := range ps {
				proxyURLs = append(proxyURLs, p.URL)
			}
		}
	}
	threads := body.Threads
	if threads < 1 {
		threads = 5
	}
	if threads > 30 {
		threads = 30
	}

	writeJSON(w, http.StatusOK, map[string]any{"runId": body.RunID, "total": len(selected)})

	req := nftStreamReq{
		runID: body.RunID, total: len(selected), chainSlug: chainSlug,
		contract: body.ContractAddress, threads: threads, selected: selected, proxyURLs: proxyURLs,
	}
	// Stream over the WS (no 30s HTTP cap): each wallet's NFTs are pushed as fetched, and
	// rate-limited wallets are retried in later rounds until none remain — so a 200-wallet
	// fetch reaches ~100% instead of timing out partway.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.streamNftItems(ctx, req)
	}()
}

func (s *Server) streamNftItems(ctx context.Context, req nftStreamReq) {
	pub := func(res nftStreamResult) {
		res.RunID = req.runID
		res.Total = req.total
		s.hub.Publish("nft", res)
	}
	slug, _ := s.osc.ContractSlug(ctx, req.chainSlug, req.contract)

	// One opensea client per proxy URL (reused), so wallets spread across IPs.
	oscByProxy := map[string]*opensea.Client{}
	for _, px := range req.proxyURLs {
		if _, ok := oscByProxy[px]; !ok {
			oscByProxy[px] = opensea.NewWithClient(wlHTTPClient(px))
		}
	}
	pick := func(i int) *opensea.Client {
		if len(req.proxyURLs) == 0 {
			return s.osc
		}
		return oscByProxy[req.proxyURLs[i%len(req.proxyURLs)]]
	}

	pending := make([]int, len(req.selected))
	for i := range pending {
		pending[i] = i
	}
	const maxRounds = 10
	for round := 0; round < maxRounds && len(pending) > 0; round++ {
		if ctx.Err() != nil {
			break
		}
		if round > 0 { // cool-down between retry rounds, longer each time
			wait := time.Duration(round) * 2 * time.Second
			if wait > 12*time.Second {
				wait = 12 * time.Second
			}
			if !wlSleep(ctx, wait) {
				break
			}
		}
		var next []int
		var mu sync.Mutex
		sem := make(chan struct{}, req.threads)
		var wg sync.WaitGroup
		for _, idx := range pending {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					mu.Lock()
					next = append(next, i)
					mu.Unlock()
					return
				}
				sw := req.selected[i]
				osc := pick(i)
				nfts, err := osc.AccountNFTs(ctx, req.chainSlug, sw.addr, slug, 200)
				if err != nil {
					mu.Lock()
					next = append(next, i) // retry this wallet next round
					mu.Unlock()
					return
				}
				listed := osc.MakerListedTokenIDs(ctx, req.chainSlug, slug, sw.addr, req.contract)
				out := []nftItem{}
				for _, n := range nfts {
					if slug == "" && !strings.EqualFold(n.Contract, req.contract) {
						continue
					}
					out = append(out, nftItem{
						WalletID: sw.id, Owner: sw.addr, TokenID: n.TokenID,
						Name: n.Name, Image: n.Image, Listed: listed[n.TokenID],
					})
				}
				pub(nftStreamResult{Index: i, WalletID: sw.id, Owner: sw.addr, Items: out, Slug: slug})
			}(idx)
		}
		wg.Wait()
		pending = next
	}
	pub(nftStreamResult{Done: true, Failed: len(pending), Slug: slug})
}

// POST /api/nft/floor {chainId, contractAddress, slug?} — collection floor price (ETH).
func (s *Server) handleNftFloor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID         int    `json:"chainId"`
		ContractAddress string `json:"contractAddress"`
		Slug            string `json:"slug"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !s.osc.HasKey() {
		writeErr(w, http.StatusBadRequest, "OpenSea API key not set")
		return
	}
	slug := strings.TrimSpace(body.Slug)
	if slug == "" {
		chainSlug, err := chains.SlugFromChainID(body.ChainID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "chain not supported by OpenSea")
			return
		}
		slug, _ = s.osc.ContractSlug(r.Context(), chainSlug, body.ContractAddress)
		if slug == "" {
			writeErr(w, http.StatusBadRequest, "couldn't resolve the collection")
			return
		}
	}
	floor, err := s.osc.Floor(r.Context(), slug)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "floor: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"floor": floor, "slug": slug})
}

// openSeaFeeRecipient is OpenSea's protocol-fee address; everything else is the creator.
const openSeaFeeRecipient = "0x0000a26b00c1f0df003000390027140000faa719"

// POST /api/nft/fees {chainId, contractAddress, slug?} — enforced platform + creator fees (bps).
func (s *Server) handleNftFees(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID         int    `json:"chainId"`
		ContractAddress string `json:"contractAddress"`
		Slug            string `json:"slug"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !s.osc.HasKey() {
		writeErr(w, http.StatusBadRequest, "OpenSea API key not set")
		return
	}
	slug := strings.TrimSpace(body.Slug)
	if slug == "" {
		chainSlug, err := chains.SlugFromChainID(body.ChainID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "chain not supported by OpenSea")
			return
		}
		slug, _ = s.osc.ContractSlug(r.Context(), chainSlug, body.ContractAddress)
		if slug == "" {
			writeErr(w, http.StatusBadRequest, "couldn't resolve the collection")
			return
		}
	}
	osFees, err := s.osc.CollectionFees(r.Context(), slug)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fees: "+err.Error())
		return
	}
	var platform, creator int64
	for _, f := range osFees {
		if strings.EqualFold(f.Recipient, openSeaFeeRecipient) {
			platform += f.Bps
		} else {
			creator += f.Bps
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"platformBps": platform, "creatorBps": creator, "feeBps": platform + creator, "slug": slug})
}

// POST /api/nft/list {chainId, contractAddress, priceWei, durationSec, items:[{walletId,tokenId,priceWei}]}
// Signs a Seaport listing per item (off-chain) and posts to OpenSea. Wallets missing
// the one-time conduit approval get an approval task instead (run it, then list again).
func (s *Server) handleNftList(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID         int    `json:"chainId"`
		ContractAddress string `json:"contractAddress"`
		PriceWei        string `json:"priceWei"` // fallback price for items without their own
		DurationSec     int64  `json:"durationSec"`
		Items           []struct {
			WalletID int64  `json:"walletId"`
			TokenID  string `json:"tokenId"`
			PriceWei string `json:"priceWei"` // per-item price (overrides the fallback)
		} `json:"items"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !s.osc.HasKey() {
		writeErr(w, http.StatusBadRequest, "OpenSea API key not set")
		return
	}
	fallbackPrice, _ := new(big.Int).SetString(strings.TrimSpace(body.PriceWei), 10)
	if body.DurationSec <= 0 {
		body.DurationSec = 30 * 86400
	}
	if !common.IsHexAddress(body.ContractAddress) {
		writeErr(w, http.StatusBadRequest, "invalid contract")
		return
	}
	chainSlug, err := chains.SlugFromChainID(body.ChainID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "chain not supported by OpenSea")
		return
	}
	contract := common.HexToAddress(body.ContractAddress)
	client, err := s.clientForChain(r, body.ChainID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	slug, _ := s.osc.ContractSlug(r.Context(), chainSlug, body.ContractAddress)
	osFees, _ := s.osc.CollectionFees(r.Context(), slug)
	fees := make([]evm.Fee, 0, len(osFees))
	for _, f := range osFees {
		fees = append(fees, evm.Fee{Recipient: f.Recipient, Bps: f.Bps})
	}

	type listItem struct {
		tokenID string
		price   *big.Int
	}
	byWallet := map[int64][]listItem{}
	failedPrice := 0
	for _, it := range body.Items {
		p := fallbackPrice
		if pw := strings.TrimSpace(it.PriceWei); pw != "" {
			if v, ok := new(big.Int).SetString(pw, 10); ok {
				p = v
			}
		}
		if p == nil || p.Sign() <= 0 {
			failedPrice++ // no valid price for this item
			continue
		}
		byWallet[it.WalletID] = append(byWallet[it.WalletID], listItem{it.TokenID, p})
	}
	listed, failed, needApproval := 0, failedPrice, 0
	var approveTasks []int64
	var firstErr string
	keepErr := func(e string) {
		if firstErr == "" {
			firstErr = e
		}
	}
	for walletID, litems := range byWallet {
		key, owner, err := s.walletKey(walletID)
		if err != nil {
			failed += len(litems)
			keepErr("wallet " + err.Error())
			continue
		}
		approved, _ := evm.IsApprovedForConduit(r.Context(), client, contract, owner)
		if !approved {
			id, _ := s.eng.Create(engine.TaskConfig{
				Group: "NFT-Approve", ChainID: body.ChainID, ContractAddress: contract.Hex(),
				Mode: engine.ModeAction, FunctionSig: "setApprovalForAll(address,bool)",
				Params: []string{evm.OSConduit, "true"}, Gas: autoGas(), WalletIDs: []int64{walletID},
			})
			if id != 0 {
				approveTasks = append(approveTasks, id)
				_ = s.eng.Start(id) // auto-run the one-time approval; re-list once it mines
			}
			needApproval += len(litems)
			wipeECDSA(key)
			continue
		}
		counter, cerr := evm.SeaportCounter(r.Context(), client, owner)
		if cerr != nil {
			failed += len(litems)
			keepErr("counter: " + cerr.Error())
			wipeECDSA(key)
			continue
		}
		for _, li := range litems {
			tokenID, ok := new(big.Int).SetString(li.tokenID, 10)
			if !ok {
				failed++
				keepErr("bad token id " + li.tokenID)
				continue
			}
			lst, berr := evm.BuildAndSignListing(key, body.ChainID, counter, contract, tokenID, li.price, fees, body.DurationSec, time.Now().Unix(), randSalt())
			if berr != nil {
				failed++
				keepErr("sign: " + berr.Error())
				continue
			}
			if perr := s.osc.PostListing(r.Context(), chainSlug, lst); perr != nil {
				s.log.API(logger.WARN, "opensea post listing failed", map[string]any{"err": perr.Error()})
				failed++
				keepErr(perr.Error())
				continue
			}
			listed++
		}
		wipeECDSA(key)
	}
	writeJSON(w, http.StatusOK, map[string]any{"listed": listed, "failed": failed, "needApproval": needApproval, "approveTasks": approveTasks, "error": firstErr})
}

// POST /api/nft/cancel {chainId, contractAddress, items:[{walletId,tokenId}]}
// Creates a Seaport incrementCounter task per distinct wallet (cancels ALL that
// wallet's open Seaport orders). Run the tasks in the Tasks tab.
func (s *Server) handleNftCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID int `json:"chainId"`
		Items   []struct {
			WalletID int64  `json:"walletId"`
			TokenID  string `json:"tokenId"`
		} `json:"items"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	seen := map[int64]bool{}
	created := 0
	for _, it := range body.Items {
		if seen[it.WalletID] {
			continue
		}
		seen[it.WalletID] = true
		id, err := s.eng.Create(engine.TaskConfig{
			Group: "NFT-Cancel", ChainID: body.ChainID, ContractAddress: evm.Seaport16,
			Mode: engine.ModeAction, FunctionSig: "incrementCounter()", Params: []string{},
			Gas: autoGas(), WalletIDs: []int64{it.WalletID},
		})
		if err == nil && id != 0 {
			created++
			_ = s.eng.Start(id) // auto-run: actually send the on-chain cancel now
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cancelled": created,
		"note":      "running incrementCounter per wallet — cancels ALL that wallet's open Seaport listings on-chain",
	})
}

// POST /api/nft/resolve {chainId, contractAddress} — read the live SeaDrop public drop.
func (s *Server) handleNftResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID         int    `json:"chainId"`
		ContractAddress string `json:"contractAddress"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !common.IsHexAddress(body.ContractAddress) {
		writeErr(w, http.StatusBadRequest, "invalid contract address")
		return
	}
	client, err := s.clientForChain(r, body.ChainID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	contract := common.HexToAddress(body.ContractAddress)
	if !evm.IsSeaDropMintable(r.Context(), client, contract) {
		writeJSON(w, http.StatusOK, map[string]any{"seadrop": false, "name": evm.CollectionName(r.Context(), client, contract)})
		return
	}
	res, err := evm.ResolveSeaDrop(r.Context(), client, contract, 1, common.Address{})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "resolve: "+err.Error())
		return
	}
	now := uint64(time.Now().Unix())
	active := res.Drop.StartTime <= now && (res.Drop.EndTime == 0 || now <= res.Drop.EndTime)
	writeJSON(w, http.StatusOK, map[string]any{
		"seadrop":      true,
		"name":         evm.CollectionName(r.Context(), client, contract),
		"priceWei":     res.Drop.MintPrice.String(),
		"maxPerWallet": res.Drop.MaxTotalMintableByWallet,
		"feeBps":       res.Drop.FeeBps,
		"feeRecipient": res.FeeRecipient.Hex(),
		"startTime":    res.Drop.StartTime,
		"endTime":      res.Drop.EndTime,
		"active":       active,
		"seaDropAddr":  evm.SeaDropAddress,
	})
}
