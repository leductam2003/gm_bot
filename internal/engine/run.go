package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"zyperbot/internal/chains"
	"zyperbot/internal/evm"
	"zyperbot/internal/logger"
	"zyperbot/internal/store"
)

type loadedWallet struct {
	id   int64
	addr common.Address
	key  *ecdsa.PrivateKey
}

// Start launches a task in its own goroutine. Returns immediately.
func (e *Engine) Start(id int64) error {
	e.mu.Lock()
	rt := e.tasks[id]
	e.mu.Unlock()
	if rt == nil {
		return ErrNotFound
	}
	rt.mu.Lock()
	if rt.Status == "running" {
		rt.mu.Unlock()
		return ErrRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	rt.Status = "running"
	cfg := rt.Config
	rt.mu.Unlock()

	go e.runTask(ctx, rt, cfg)
	return nil
}

func (e *Engine) runTask(ctx context.Context, rt *TaskRuntime, cfg TaskConfig) {
	_ = e.st.UpdateTaskStatus(cfg.ID, "running")
	e.log.Task(logger.INFO, "task started", cfg.ID, map[string]any{"mode": cfg.Mode})

	wallets, err := e.loadWallets(cfg)
	if err != nil {
		rt.setStatus("done")
		e.log.Task(logger.ERROR, "load wallets failed: "+err.Error(), cfg.ID, nil)
		_ = e.st.UpdateTaskStatus(cfg.ID, "idle")
		e.emit(rt)
		return
	}
	defer zeroKeys(wallets)

	// Resolve the proxy group once per run (rotated per wallet for OpenSea polling).
	rt.proxies = nil
	if cfg.ProxyGroup != "" {
		if ps, perr := e.st.ListProxiesByGroup(cfg.ProxyGroup); perr == nil {
			for _, p := range ps {
				rt.proxies = append(rt.proxies, p.URL)
			}
		}
	}

	// init statuses
	for _, lw := range wallets {
		id := lw.id
		addr := lw.addr.Hex()
		rt.setWallet(id, func(w *WalletStatus) { w.Address = addr; w.Status = "idle"; w.TxHash = ""; w.Detail = "" })
	}
	e.emit(rt)

	// Snipe timing: wait until StartAt before firing (Action/Spam). Coarse wait so
	// Stop stays responsive; the actual send happens right when the stage opens.
	if cfg.StartAt > 0 && cfg.Mode != ModeSimulate {
		for _, lw := range wallets {
			id := lw.id
			rt.setWallet(id, func(w *WalletStatus) { w.Status = "waiting"; w.Detail = "waiting for start time" })
		}
		e.emit(rt)
		e.log.Task(logger.INFO, fmt.Sprintf("waiting until %d before firing", cfg.StartAt), cfg.ID, nil)
		if !waitUntil(ctx, cfg.StartAt) {
			rt.setStatus("stopped")
			rt.mu.Lock()
			for _, w := range rt.Wallets {
				if w.Status == "waiting" {
					w.Status = "stopped"
				}
			}
			rt.mu.Unlock()
			_ = e.st.UpdateTaskStatus(cfg.ID, "idle")
			e.log.Task(logger.INFO, "task stopped while waiting", cfg.ID, nil)
			e.emit(rt)
			return
		}
	}

	value := parseBigOr0(cfg.ValueWei)

	switch cfg.Mode {
	case ModeSimulate, ModeAction:
		e.runPass(ctx, rt, cfg, wallets, value)
	case ModeSpam:
		for ctx.Err() == nil {
			e.runPass(ctx, rt, cfg, wallets, value)
			if cfg.DelayMs > 0 {
				sleepCtx(ctx, time.Duration(cfg.DelayMs)*time.Millisecond)
			}
		}
	default:
		e.log.Task(logger.ERROR, "unknown mode "+string(cfg.Mode), cfg.ID, nil)
	}

	final := "done"
	if ctx.Err() != nil {
		final = "stopped"
		// mark still-running wallets as stopped
		rt.mu.Lock()
		for _, w := range rt.Wallets {
			if w.Status == "running" || w.Status == "idle" {
				w.Status = "stopped"
			}
		}
		rt.mu.Unlock()
	}
	rt.setStatus(final)
	_ = e.st.UpdateTaskStatus(cfg.ID, "idle")
	e.log.Task(logger.INFO, "task "+final, cfg.ID, nil)
	e.emit(rt)
	e.notifyDiscord(rt, cfg, final)
}

// runPass runs every wallet once, all concurrently (one goroutine per wallet — no
// batch cap), then waits for them to finish.
func (e *Engine) runPass(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, wallets []loadedWallet, value *big.Int) {
	// Resolve gas fees once per pass (shared across wallets) to avoid RPC 429. On
	// failure we log and fall back to per-wallet resolution in runOne. All modes send
	// now (Simulate = eth_call then send), so always resolve fees.
	var shared evm.ResolvedFees
	if nodes, err := e.resolveNodes(ctx, cfg); err != nil {
		e.log.Task(logger.WARN, "shared gas: rpc resolve failed, per-wallet fallback: "+err.Error(), cfg.ID, nil)
	} else if fees, ferr := evm.ResolveFees(ctx, nodes[0].Client, cfg.Gas); ferr != nil {
		e.log.Task(logger.WARN, "shared gas: resolveFees failed, per-wallet fallback: "+ferr.Error(), cfg.ID, nil)
	} else {
		shared = fees
	}

	if ctx.Err() != nil {
		return
	}
	var wg sync.WaitGroup
	for _, lw := range wallets {
		wg.Add(1)
		go func(lw loadedWallet) {
			defer wg.Done()
			e.runOne(ctx, rt, cfg, value, shared, lw)
		}(lw)
	}
	wg.Wait()
}

func (e *Engine) runOne(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, value *big.Int, shared evm.ResolvedFees, lw loadedWallet) {
	rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running"; w.Detail = "" })
	e.emit(rt)

	nodes, err := e.resolveNodes(ctx, cfg)
	if err != nil {
		e.failWallet(rt, lw, "rpc: "+err.Error())
		return
	}
	client := nodes[0].Client

	to := common.HexToAddress(cfg.ContractAddress)
	var data []byte
	if cfg.Seadrop {
		qty := cfg.Quantity
		if qty < 1 {
			qty = 1
		}
		// Prefer the OpenSea voucher: a server-signed mint tx that works for BOTH the
		// public AND allowlist/FCFS stages (OpenSea returns the active eligible stage
		// for this wallet). Falls back to on-chain mintPublic if the voucher is
		// unavailable (e.g. stage not open yet, or GraphQL hash rotated).
		chainSlug, _ := chains.SlugFromChainID(cfg.ChainID)
		proxyURL := ""
		if n := len(rt.proxies); n > 0 {
			proxyURL = rt.proxies[int(lw.id)%n] // rotate proxy per wallet
		}
		v, verr := e.osc.MintVoucher(ctx, lw.addr.Hex(), to.Hex(), qty, chainSlug, proxyURL)
		if verr == nil && common.IsHexAddress(v.To) && len(common.FromHex(v.Data)) > 0 {
			to = common.HexToAddress(v.To)
			data = common.FromHex(v.Data)
			value = parseBigOr0(v.ValueWei)
		} else {
			var feeOverride common.Address
			if cfg.FeeRecipient != "" {
				feeOverride = common.HexToAddress(cfg.FeeRecipient)
			}
			r, rerr := evm.ResolveSeaDrop(ctx, client, to, qty, feeOverride)
			if rerr != nil {
				detail := "seadrop: " + rerr.Error()
				if verr != nil {
					detail = "seadrop voucher: " + verr.Error() + "; onchain: " + rerr.Error()
				}
				e.failWallet(rt, lw, detail)
				return
			}
			to, data, value = r.To, r.Data, r.Value
			// Optional per-unit price override (editable Price/NFT in the UI).
			if cfg.MintPriceWei != "" {
				if p, ok := new(big.Int).SetString(cfg.MintPriceWei, 10); ok {
					value = new(big.Int).Mul(p, big.NewInt(int64(qty)))
				}
			}
		}
	} else {
		d, derr := evm.BuildCalldata(cfg.HexMode, cfg.RawHex, cfg.FunctionSig, cfg.Params, lw.addr)
		if derr != nil {
			e.failWallet(rt, lw, "calldata: "+derr.Error())
			return
		}
		data = d
	}

	// Simulate mode = eth_call first, then execute only if it would succeed; on a
	// revert the wallet fails with the decoded reason and no gas is spent. (Action
	// mode skips this check and sends straight away.)
	if cfg.Mode == ModeSimulate {
		if ok, reason := evm.Simulate(ctx, client, lw.addr, to, data, value); !ok {
			e.failWallet(rt, lw, "simulate: "+reason)
			return
		}
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = "simulate ok, sending" })
		e.log.Tx(logger.INFO, "simulate ok, sending", cfg.ID, lw.addr.Hex(), nil)
		e.emit(rt)
	}

	// ----- Send (Action, or Simulate after a passing eth_call) -----
	fees := shared
	if fees.MaxFeePerGas == nil {
		f, ferr := evm.ResolveFees(ctx, client, cfg.Gas)
		if ferr != nil {
			e.failWallet(rt, lw, "gas: "+ferr.Error())
			return
		}
		fees = f
	}

	// gas limit
	var gasLimit uint64
	if cfg.Gas.GasLimit != nil {
		gasLimit = *cfg.Gas.GasLimit
	} else {
		pad := evm.DefaultEstimatePad
		if cfg.Gas.EstimatePad != nil {
			pad = *cfg.Gas.EstimatePad
		}
		gl, gerr := evm.EstimateGasLimit(ctx, client, ethereum.CallMsg{From: lw.addr, To: &to, Data: data, Value: value}, pad)
		if gerr != nil {
			e.failWallet(rt, lw, "estimateGas: "+gerr.Error())
			return
		}
		gasLimit = gl
	}

	// nonce
	var nonce uint64
	if cfg.NonceOverride != nil {
		nonce = *cfg.NonceOverride
	} else {
		n, nerr := e.nonce.Reserve(ctx, client, lw.addr)
		if nerr != nil {
			e.failWallet(rt, lw, "nonce: "+nerr.Error())
			return
		}
		nonce = n
	}

	// balance guard: gasLimit*maxFee + value
	gasReserve := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), fees.MaxFeePerGas)
	needed := new(big.Int).Add(value, gasReserve)
	if bal, berr := client.BalanceAt(ctx, lw.addr, nil); berr == nil && bal.Cmp(needed) < 0 {
		e.nonce.Invalidate(lw.addr)
		e.failWallet(rt, lw, fmt.Sprintf("insufficient funds: need %s wei, have %s", needed, bal))
		return
	}
	gasFeeEth := weiToEth(gasReserve)
	rt.setWallet(lw.id, func(w *WalletStatus) { w.GasFee = gasFeeEth })

	chainID := big.NewInt(int64(cfg.ChainID))

	// Flashbots private bundle (ETH mainnet only): never hits the public mempool, so
	// the mint can't be front-run. Falls through to public broadcast on other chains.
	if cfg.Flashbots && cfg.ChainID == 1 {
		// Apply Flashbots tuning from Settings: when the task's fees are "auto", override
		// priority/max with the configured bundle fees (builders pick by economics).
		ac := e.appConfig()
		fbFees := fees
		if cfg.Gas.Mode == evm.GasAuto || cfg.Gas.Mode == "" {
			if pr := numCfg(ac, "fbPriorityGwei"); pr > 0 {
				fbFees.MaxPriorityFeePerGas = gweiToWei(pr)
			}
			if mx := numCfg(ac, "fbMaxFeeGwei"); mx > 0 {
				fbFees.MaxFeePerGas = gweiToWei(mx)
			}
		}
		tx, serr := evm.SignTx(lw.key, evm.TxRequest{
			ChainID: chainID, Nonce: nonce, To: to, Data: data, Value: value,
			GasLimit: gasLimit, MaxFeePerGas: fbFees.MaxFeePerGas, MaxPriority: fbFees.MaxPriorityFeePerGas,
		})
		if serr != nil {
			e.failWallet(rt, lw, "sign: "+serr.Error())
			return
		}
		head, herr := client.HeaderByNumber(ctx, nil)
		if herr != nil || head.Number == nil {
			// Without the head block we can't target a future block — a target of 0
			// would be silently accepted and never included. Fail loudly instead.
			e.failWallet(rt, lw, "flashbots: cannot read head block: "+errStr(herr))
			return
		}
		target := head.Number.Uint64() + 1
		blocks := 4
		if wb := int(numCfg(ac, "fbWindowBlocks")); wb > 0 {
			blocks = wb
		}
		if ferr := evm.SubmitFlashbots(ctx, tx, target, blocks); ferr != nil {
			e.nonce.Invalidate(lw.addr)
			e.failWallet(rt, lw, "flashbots: "+ferr.Error())
			return
		}
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running"; w.TxHash = tx.Hash().Hex(); w.RPC = "flashbots"; w.Detail = "private bundle, waiting" })
		e.log.Tx(logger.INFO, "flashbots bundle", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": tx.Hash().Hex(), "targetBlock": target, "nonce": nonce})
		e.emit(rt)
		e.watchAndBump(ctx, rt, cfg, nodes, lw, nonce, gasLimit, value, data, to, fees, tx.Hash())
		return
	}

	// broadcast with retry (already-known / nonce error)
	curNonce := nonce
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "stopped" })
			e.emit(rt)
			return
		}
		tx, serr := evm.SignTx(lw.key, evm.TxRequest{
			ChainID: chainID, Nonce: curNonce, To: to, Data: data, Value: value,
			GasLimit: gasLimit, MaxFeePerGas: fees.MaxFeePerGas, MaxPriority: fees.MaxPriorityFeePerGas,
		})
		if serr != nil {
			e.failWallet(rt, lw, "sign: "+serr.Error())
			return
		}
		res, berr := evm.Broadcast(ctx, tx, nodes, cfg.MultiRpc)
		if berr == nil {
			rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running"; w.TxHash = res.TxHash.Hex(); w.RPC = shortURL(res.RPC); w.Detail = "broadcast, waiting" })
			// Log host only — the RPC URL may embed a secret token.
			e.log.Tx(logger.INFO, "broadcast", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": res.TxHash.Hex(), "rpc": shortURL(res.RPC), "nonce": curNonce})
			e.emit(rt)
			e.watchAndBump(ctx, rt, cfg, nodes, lw, curNonce, gasLimit, value, data, to, fees, res.TxHash)
			return
		}
		msg := berr.Error()
		if evm.IsAlreadyKnown(msg) {
			rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "success"; w.TxHash = tx.Hash().Hex(); w.Detail = "already known (mempool)" })
			e.log.Tx(logger.INFO, "already known", cfg.ID, lw.addr.Hex(), nil)
			e.emit(rt)
			return
		}
		if evm.IsNonceError(msg) && attempt < 2 {
			if fresh, ferr := client.PendingNonceAt(ctx, lw.addr); ferr == nil {
				e.log.Tx(logger.WARN, fmt.Sprintf("nonce conflict, refetch %d (attempt %d)", fresh, attempt+2), cfg.ID, lw.addr.Hex(), nil)
				curNonce = fresh
				continue
			}
		}
		e.nonce.Invalidate(lw.addr)
		e.failWallet(rt, lw, "broadcast: "+msg)
		return
	}
	e.failWallet(rt, lw, "broadcast failed after retries")
}

// watchAndBump polls for the receipt; on a Boost (pump epoch increase) it re-signs
// the same nonce with higher gas and rebroadcasts. Port of watchAndBumpMempool.
func (e *Engine) watchAndBump(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, nodes []evm.Node, lw loadedWallet,
	nonce uint64, gasLimit uint64, value *big.Int, data []byte, to common.Address, fees evm.ResolvedFees, txHash common.Hash) {

	client := nodes[0].Client
	deadline := time.Now().Add(90 * time.Second)
	lastEpoch := rt.pumpEpoch.Load()
	chainID := big.NewInt(int64(cfg.ChainID))

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			rt.setWallet(lw.id, func(w *WalletStatus) {
				if w.Status == "running" {
					w.Status = "stopped"
				}
			})
			e.emit(rt)
			return
		}
		if rcpt, err := client.TransactionReceipt(ctx, txHash); err == nil && rcpt != nil {
			if rcpt.Status == 1 {
				rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "success"; w.Detail = fmt.Sprintf("mined block %d", rcpt.BlockNumber.Uint64()) })
				e.log.Tx(logger.INFO, "mined", cfg.ID, lw.addr.Hex(), map[string]any{"block": rcpt.BlockNumber.Uint64(), "txHash": txHash.Hex()})
				// Chained follow-up after a successful action (transfer minted NFT / drain ETH / ...).
				if cfg.Mode == ModeAction && cfg.PostAction != nil && cfg.PostAction.Type != "" && cfg.PostAction.Type != "none" {
					e.runPostAction(ctx, rt, cfg, nodes, lw, rcpt)
				}
			} else {
				rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "failed"; w.Detail = "reverted on-chain" })
				e.log.Tx(logger.WARN, "reverted on-chain", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": txHash.Hex()})
			}
			e.emit(rt)
			return
		}
		// Boost?
		if epoch := rt.pumpEpoch.Load(); epoch > lastEpoch {
			lastEpoch = epoch
			// Read the LIVE baseFee so a fee spike during the wait is covered
			// (matches mintEngine.ts watchAndBumpMempool).
			baseFee := big.NewInt(0)
			if head, herr := client.HeaderByNumber(ctx, nil); herr == nil && head.BaseFee != nil {
				baseFee = head.BaseFee
			}
			fees = bumpFees(fees, baseFee)
			if tx, serr := evm.SignTx(lw.key, evm.TxRequest{
				ChainID: chainID, Nonce: nonce, To: to, Data: data, Value: value,
				GasLimit: gasLimit, MaxFeePerGas: fees.MaxFeePerGas, MaxPriority: fees.MaxPriorityFeePerGas,
			}); serr == nil {
				if res, berr := evm.Broadcast(ctx, tx, nodes, true); berr == nil {
					txHash = res.TxHash
					rt.setWallet(lw.id, func(w *WalletStatus) { w.TxHash = txHash.Hex(); w.Detail = "pumped gas" })
					e.log.Tx(logger.INFO, "pump gas", cfg.ID, lw.addr.Hex(), map[string]any{"maxFee": fees.MaxFeePerGas.String()})
					e.emit(rt)
				}
			}
		}
		sleepCtx(ctx, 1500*time.Millisecond)
	}
	// Still pending — leave as running/pending (don't auto-pump).
	rt.setWallet(lw.id, func(w *WalletStatus) {
		if w.Status == "running" {
			w.Detail = "pending >90s"
		}
	})
	e.emit(rt)
}

// bumpFees is the exact port of the pump branch in mintEngine.ts watchAndBumpMempool:
//
//	newPrio = max(prio*2, prio+0.3gwei), floored at prio*1.125 (geth's replacement min)
//	newMax  = baseFee*2 + newPrio,       floored at maxFee*1.125
//
// Using the LIVE baseFee (not the stale maxFee) keeps replacements valid through a
// fee spike — a stale maxFee*1.125 would be far too low when baseFee jumps.
func bumpFees(fees evm.ResolvedFees, baseFee *big.Int) evm.ResolvedFees {
	mul1125 := func(v *big.Int) *big.Int {
		out := new(big.Int).Mul(v, big.NewInt(1125))
		return out.Div(out, big.NewInt(1000))
	}
	prio := fees.MaxPriorityFeePerGas
	dbl := new(big.Int).Mul(prio, big.NewInt(2))
	plus := new(big.Int).Add(prio, big.NewInt(300_000_000)) // +0.3 gwei
	newPrio := dbl
	if plus.Cmp(newPrio) > 0 {
		newPrio = plus
	}
	if floorP := mul1125(prio); newPrio.Cmp(floorP) < 0 {
		newPrio = floorP
	}
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	newMax := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), newPrio)
	if floorM := mul1125(fees.MaxFeePerGas); newMax.Cmp(floorM) < 0 {
		newMax = floorM
	}
	return evm.ResolvedFees{MaxFeePerGas: newMax, MaxPriorityFeePerGas: newPrio}
}

// --- helpers ---

func (e *Engine) failWallet(rt *TaskRuntime, lw loadedWallet, reason string) {
	rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "failed"; w.Detail = reason })
	e.log.Tx(logger.WARN, "failed: "+reason, rt.Config.ID, lw.addr.Hex(), nil)
	e.emit(rt)
}

func (e *Engine) loadWallets(cfg TaskConfig) ([]loadedWallet, error) {
	rows, err := e.st.ListWallets()
	if err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, id := range cfg.WalletIDs {
		want[id] = true
	}
	var out []loadedWallet
	for _, r := range rows {
		if r.Network != "evm" {
			continue
		}
		if len(want) > 0 && !want[r.ID] {
			continue
		}
		pkBytes, derr := e.vault.Open(r.EncPrivKey)
		if derr != nil {
			return nil, fmt.Errorf("decrypt wallet %d: %w", r.ID, derr)
		}
		// Decode hex → 32 raw bytes WITHOUT an intermediate Go string (strings are
		// immutable and cannot be wiped; a key copy would linger in the heap).
		trimmed := bytes.TrimPrefix(bytes.TrimSpace(pkBytes), []byte("0x"))
		raw := make([]byte, hex.DecodedLen(len(trimmed)))
		_, hxerr := hex.Decode(raw, trimmed)
		var key *ecdsa.PrivateKey
		var kerr error
		if hxerr != nil {
			kerr = hxerr
		} else {
			key, kerr = gethcrypto.ToECDSA(raw)
		}
		// Wipe every plaintext copy of the key material immediately.
		for i := range pkBytes {
			pkBytes[i] = 0
		}
		for i := range raw {
			raw[i] = 0
		}
		if kerr != nil {
			wipeKey(key)
			return nil, fmt.Errorf("parse wallet %d key: %w", r.ID, kerr)
		}
		out = append(out, loadedWallet{id: r.ID, addr: gethcrypto.PubkeyToAddress(key.PublicKey), key: key})
	}
	if len(out) == 0 {
		return nil, errors.New("no EVM wallets selected")
	}
	return out, nil
}

func zeroKeys(ws []loadedWallet) {
	for _, w := range ws {
		wipeKey(w.key)
	}
}

// wipeKey zeroes the actual backing memory of the private scalar. big.Int.Bits()
// exposes the underlying []Word, so zeroing it wipes the real bytes (SetInt64(0)
// alone leaves the old limbs in freed memory).
func wipeKey(key *ecdsa.PrivateKey) {
	if key == nil || key.D == nil {
		return
	}
	bits := key.D.Bits()
	for i := range bits {
		bits[i] = 0
	}
	key.D.SetInt64(0)
}

var _ = store.Wallet{} // keep store import explicit

func parseBigOr0(s string) *big.Int {
	s = strings.TrimSpace(s)
	if s == "" {
		return big.NewInt(0)
	}
	if n, ok := new(big.Int).SetString(s, 10); ok {
		return n
	}
	return big.NewInt(0)
}

func weiToEth(wei *big.Int) string {
	f := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18))
	return f.Text('f', 6)
}

func shortURL(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i > 0 {
		return u[:i]
	}
	return u
}

func errStr(err error) string {
	if err == nil {
		return "nil head"
	}
	return err.Error()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// waitUntil blocks until the given unix second arrives, returning false if the task
// is cancelled first. Coarse 2s steps keep Stop responsive without busy-waiting.
func waitUntil(ctx context.Context, unixSec int64) bool {
	for {
		now := time.Now().Unix()
		if now >= unixSec {
			return true
		}
		step := time.Duration(unixSec-now) * time.Second
		if step > 2*time.Second {
			step = 2 * time.Second
		}
		t := time.NewTimer(step)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}
