package api

import (
	"context"
	"math/big"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"zyperbot/internal/chains"
	"zyperbot/internal/evm"
	"zyperbot/internal/logger"
	"zyperbot/internal/store"
)

// salesSyncing guards against overlapping passes (the boot pass + a manual Sync) hammering
// the RPC at once.
var salesSyncing atomic.Bool

const (
	salesLookback  = 432000 // blocks to scan back on first run (~8 weeks on Ethereum)
	salesChunk     = 9000   // max block span per eth_getLogs (well under the RPC range limit)
	salesMaxChunks = 60     // cap work per pass (60*9000 covers the full first-run lookback)
	sellerBatch    = 100    // wallets per getLogs topic filter
	mintSearchSpan = 450000 // how far before a sale to look for its mint (under the ~500k RPC limit)
)

// RunSalesSync detects sales of minted NFTs straight from on-chain Seaport logs (no
// marketplace API) so a LISTING that sold off-app shows up as realized PNL. It only ever
// books a sale that actually happened on-chain. Runs an initial pass at boot, then every
// few minutes.
func (s *Server) RunSalesSync(ctx context.Context) {
	s.syncSalesOnce(ctx)
	t := time.NewTicker(3 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncSalesOnce(ctx)
		}
	}
}

// syncSalesOnce scans Seaport OrderFulfilled logs (offerer = one of our wallets) per chain
// we've minted on, books any confirmed sale of a token we minted, and returns the count.
// Dedup by tx so an Accept-Offer sale is never double-counted.
func (s *Server) syncSalesOnce(ctx context.Context) int {
	if !salesSyncing.CompareAndSwap(false, true) {
		return 0 // a pass is already running
	}
	defer salesSyncing.Store(false)
	wallets, _ := s.st.ListWallets()
	sellers := make([]common.Address, 0, len(wallets))
	for _, w := range wallets {
		if w.Network == "evm" && common.IsHexAddress(w.Address) {
			sellers = append(sellers, common.HexToAddress(w.Address))
		}
	}
	if len(sellers) == 0 {
		return 0
	}
	// Scan Ethereum mainnet (where listings sell) plus any chain we already have mints on —
	// the backfill discovers mints from sales, so it must run even with an empty mints table.
	chainSet := map[int]bool{1: true}
	if contracts, err := s.st.MintedContracts(); err == nil {
		for _, c := range contracts {
			chainSet[c.ChainID] = true
		}
	}
	booked := 0
	nameCache := map[string]string{}
	for chainID := range chainSet {
		if ctx.Err() != nil {
			break
		}
		nodes, nerr := s.nodesForChainCtx(ctx, chainID)
		if nerr != nil || len(nodes) == 0 {
			continue
		}
		client := nodes[0].Client
		latest, lerr := client.BlockNumber(ctx)
		if lerr != nil {
			continue
		}
		from := s.salesFromBlock(chainID, latest)
		n, scannedTo := s.scanChainSales(ctx, chainID, client, sellers, from, latest, nameCache)
		booked += n
		s.setSalesLastBlock(chainID, scannedTo) // resume here next pass (deep backfill spans passes)
	}
	if booked > 0 {
		s.log.API(logger.INFO, "sales sync booked on-chain sales", map[string]any{"new": booked})
		s.hub.Publish("home", map[string]any{"salesBooked": booked})
	}
	return booked
}

// scanChainSales walks [from,to] in chunks, books each confirmed sale of a token our wallet
// minted, and returns the count plus the highest block it actually reached (so a capped pass
// resumes there next time instead of skipping the gap).
func (s *Server) scanChainSales(ctx context.Context, chainID int, client *ethclient.Client, sellers []common.Address, from, to uint64, nameCache map[string]string) (int, uint64) {
	booked, chunks := 0, 0
	end := to
	lowest := to
	reached := false
	for end >= from && chunks < salesMaxChunks {
		if ctx.Err() != nil {
			break
		}
		chunks++
		start := from
		if end-from > salesChunk {
			start = end - salesChunk
		}
		// One bad chunk shouldn't hide the rest (esp. recent sales, which come first).
		if sales, err := s.scanSalesWindow(ctx, client, sellers, start, end); err == nil {
			for _, sale := range sales {
				if s.bookSale(ctx, chainID, client, sale, nameCache) {
					booked++
				}
			}
		}
		lowest = start
		if start == from {
			reached = true
			break
		}
		end = start - 1
	}
	// Recent sales are always covered (we swept newest→oldest). If the per-pass cap stopped us
	// before reaching `from`, surface it instead of silently skipping the older gap.
	if !reached && ctx.Err() == nil {
		s.log.API(logger.WARN, "sales scan hit per-pass cap; older blocks not scanned this pass", map[string]any{"chain": chainID, "scannedDownTo": lowest, "wantedFrom": from})
	}
	// Advance the cursor to the newest edge so the next pass is incremental.
	return booked, to
}

// scanSalesWindow batches the wallet list across the seller topic filter (some RPCs cap the
// number of topic values) and retries transient RPC errors before giving up on the window.
func (s *Server) scanSalesWindow(ctx context.Context, client *ethclient.Client, sellers []common.Address, start, end uint64) ([]evm.OnchainSale, error) {
	var all []evm.OnchainSale
	for i := 0; i < len(sellers); i += sellerBatch {
		j := i + sellerBatch
		if j > len(sellers) {
			j = len(sellers)
		}
		var sales []evm.OnchainSale
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if sales, err = evm.ScanSeaportSales(ctx, client, sellers[i:j], new(big.Int).SetUint64(start), new(big.Int).SetUint64(end)); err == nil {
				break
			}
			if !sleepCtx(ctx, time.Duration(attempt+1)*time.Second) {
				return nil, ctx.Err()
			}
		}
		if err != nil {
			return nil, err
		}
		all = append(all, sales...)
	}
	return all, nil
}

// sleepCtx waits d or until ctx is done; returns false if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// bookSale records one on-chain sale as realized PNL if the seller MINTED the token (read
// from chain). Backfills the mint record (with its on-chain cost) so the activity log and
// cost basis reflect history. Returns false if already booked or the token wasn't minted.
func (s *Server) bookSale(ctx context.Context, chainID int, client *ethclient.Client, sale evm.OnchainSale, nameCache map[string]string) bool {
	if exists, _ := s.st.SaleExists(sale.TxHash, sale.Contract, sale.TokenID); exists {
		return false
	}
	cost, found := s.st.MatchMintCost(chainID, sale.Contract, sale.TokenID, sale.Seller)
	if !found {
		// Was this token minted by the seller on-chain? (minted-then-sold). If so, backfill
		// the mint with its real on-chain cost; if not (bought), skip it.
		tokenID, ok := new(big.Int).SetString(sale.TokenID, 10)
		if !ok {
			return false
		}
		saleBlk := new(big.Int).SetUint64(sale.Block)
		mintFrom := big.NewInt(0)
		if sale.Block > mintSearchSpan {
			mintFrom = new(big.Int).SetUint64(sale.Block - mintSearchSpan)
		}
		mint, minted := evm.FindMint(ctx, client, common.HexToAddress(sale.Contract), tokenID, mintFrom, saleBlk, common.HexToAddress(sale.Seller))
		if !minted {
			return false // bought, not minted (or mint older than the window) — exclude
		}
		mc, _ := evm.MintCost(ctx, client, mint.TxHash, common.HexToAddress(sale.Seller))
		_, _ = s.st.AddMint(store.Mint{
			Ts: time.Unix(s.blockTime(ctx, client, mint.Block), 0), ChainID: chainID, Contract: sale.Contract,
			TokenID: sale.TokenID, Address: sale.Seller, TxHash: mint.TxHash.Hex(), CostWei: mc, Status: "minted",
		})
		cost, _ = s.st.MatchMintCost(chainID, sale.Contract, sale.TokenID, sale.Seller) // marks sold, returns mc
	}
	_, aerr := s.st.AddSale(store.Sale{
		Ts: time.Now(), ChainID: chainID, Contract: sale.Contract, Collection: s.collectionName(ctx, chainID, sale.Contract, nameCache),
		TokenID: sale.TokenID, Address: sale.Seller, TxHash: sale.TxHash, ProceedsWei: sale.ProceedsWei, CostWei: cost,
	})
	return aerr == nil
}

// blockTime returns a block's unix timestamp (for a backfilled mint's activity time),
// falling back to "now" if the header can't be read.
func (s *Server) blockTime(ctx context.Context, client *ethclient.Client, block uint64) int64 {
	if h, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(block)); err == nil && h != nil {
		return int64(h.Time)
	}
	return time.Now().Unix()
}

// collectionName resolves a contract to its OpenSea display name (cosmetic, cached) for the
// TOP PLAYS labels — the sale and proceeds themselves are read on-chain. Falls back to a
// short contract string when OpenSea is unavailable.
func (s *Server) collectionName(ctx context.Context, chainID int, contract string, cache map[string]string) string {
	if n, ok := cache[contract]; ok {
		return n
	}
	name := abbrevHex(contract)
	if s.osc != nil && s.osc.HasKey() {
		if cs, err := chains.SlugFromChainID(chainID); err == nil {
			if slug, _ := s.osc.ContractSlug(ctx, cs, contract); slug != "" {
				if ci, e := s.osc.Collection(ctx, slug); e == nil && ci.Name != "" {
					name = ci.Name
				}
			}
		}
	}
	cache[contract] = name
	return name
}

func abbrevHex(a string) string {
	if len(a) > 12 {
		return a[:6] + "…" + a[len(a)-4:]
	}
	return a
}

func (s *Server) salesFromBlock(chainID int, latest uint64) uint64 {
	if v, err := s.st.GetSetting(salesBlockKey(chainID)); err == nil {
		if n, e := strconv.ParseUint(v, 10, 64); e == nil {
			if n+1 <= latest {
				return n + 1
			}
			return latest
		}
	}
	if latest > salesLookback {
		return latest - salesLookback
	}
	return 0
}

func (s *Server) setSalesLastBlock(chainID int, latest uint64) {
	_ = s.st.SetSetting(salesBlockKey(chainID), strconv.FormatUint(latest, 10))
}

func salesBlockKey(chainID int) string { return "sales.lastBlock." + strconv.Itoa(chainID) }

// recordSaleOnConfirm books an Accept-Offer sale only AFTER its tx confirms on-chain with
// status 1 — "only count what actually sold". Detached so it never blocks the accept flow;
// dedup keeps the on-chain scanner from also counting it.
func (s *Server) recordSaleOnConfirm(chainID int, contract common.Address, tokenID string, seller common.Address, txHash, proceedsWei, collName string, client *ethclient.Client) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		hash := common.HexToHash(txHash)
		for {
			if rcpt, err := client.TransactionReceipt(ctx, hash); err == nil && rcpt != nil {
				if rcpt.Status != 1 {
					return // reverted — don't count
				}
				if exists, _ := s.st.SaleExists(txHash, contract.Hex(), tokenID); exists {
					return
				}
				cost, found := s.st.MatchMintCost(chainID, contract.Hex(), tokenID, seller.Hex())
				if !found {
					return // not a token we minted — exclude (matches the on-chain scanner)
				}
				_, _ = s.st.AddSale(store.Sale{
					Ts: time.Now(), ChainID: chainID, Contract: contract.Hex(), Collection: collName,
					TokenID: tokenID, Address: seller.Hex(), TxHash: txHash,
					ProceedsWei: proceedsWei, CostWei: cost,
				})
				s.hub.Publish("home", map[string]any{"salesBooked": 1})
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

// POST /api/home/sync — run one on-chain sales-sync pass now (the dashboard's "Sync sales"
// button). Detached so a slow scan doesn't block the request; results arrive over the WS.
func (s *Server) handleHomeSync(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		s.syncSalesOnce(ctx)
	}()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
