package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
	// "stopping" (Stop called, runner still unwinding an RPC) must NOT block a restart —
	// otherwise a stopped task stays un-startable until its last call returns.
	if rt.Status == "running" {
		rt.mu.Unlock()
		return ErrRunning
	}
	if prev := rt.cancel; prev != nil {
		prev() // release any lingering context from a previous run
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	rt.Status = "running"
	gen := rt.runGen.Add(1) // this run's generation; the tail finalizes only if still current
	cfg := rt.Config
	rt.mu.Unlock()

	go e.runTask(ctx, rt, cfg, gen)
	return nil
}

// RunWallet runs a SINGLE wallet of a task (the per-row ▶ button), independent of the
// whole task — so clicking run on one wallet doesn't fire all of them. Spam mode loops
// that one wallet until StopWallet; other modes do one pass.
func (e *Engine) RunWallet(taskID, walletID int64) error {
	e.mu.Lock()
	rt := e.tasks[taskID]
	e.mu.Unlock()
	if rt == nil {
		return ErrNotFound
	}
	rt.mu.Lock()
	cfg := rt.Config
	// A whole-task run (Start) tracks itself in rt.cancel / rt.Status, NOT in
	// walletCancel — so without this guard a per-row ▶ during a whole-task run would
	// spawn a SECOND runner for the same wallet: two nonce reservations, double-send.
	if rt.Status == "running" || rt.Status == "stopping" {
		rt.mu.Unlock()
		return ErrRunning
	}
	if rt.walletCancel == nil {
		rt.walletCancel = map[int64]context.CancelFunc{}
	}
	if _, busy := rt.walletCancel[walletID]; busy {
		rt.mu.Unlock()
		return ErrRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.walletCancel[walletID] = cancel
	if cfg.ProxyGroup != "" && len(rt.proxies) == 0 {
		if ps, perr := e.st.ListProxiesByGroup(cfg.ProxyGroup); perr == nil {
			for _, p := range ps {
				rt.proxies = append(rt.proxies, p.URL)
			}
		}
	}
	rt.mu.Unlock()

	one := cfg
	one.WalletIDs = []int64{walletID}
	wallets, err := e.loadWallets(one)
	if err != nil || len(wallets) == 0 {
		e.dropWalletCancel(rt, walletID)
		if err == nil {
			err = errors.New("wallet not found")
		}
		return err
	}
	wcfg := cfg.effective(walletID) // honor this wallet's per-wallet override, if any
	value := parseBigOr0(wcfg.ValueWei)
	go func() {
		defer zeroKeys(wallets)
		defer e.dropWalletCancel(rt, walletID)
		lw := wallets[0]
		// Honor the task's scheduled start — a per-row ▶ on a scheduled task used to
		// send IMMEDIATELY, ignoring StartAt entirely (it fired into a closed mint).
		if wcfg.StartAt > 0 && time.Now().Unix() < wcfg.StartAt {
			rt.setWallet(walletID, func(w *WalletStatus) { w.Status = "waiting"; w.Detail = "waiting for start time" })
			e.emit(rt)
			if !waitUntil(ctx, wcfg.StartAt) {
				rt.setWallet(walletID, func(w *WalletStatus) { w.Status = "stopped"; w.Detail = "" })
				e.emit(rt)
				return
			}
		}
		if wcfg.Mode == ModeSpam {
			e.spamWallet(ctx, rt, wcfg, value, lw)
		} else {
			e.runOne(ctx, rt, wcfg, value, evm.ResolvedFees{}, lw)
		}
		if ctx.Err() != nil {
			rt.setWallet(walletID, func(w *WalletStatus) {
				if w.Status == "running" {
					w.Status = "stopped"
				}
			})
		}
		e.emit(rt)
	}()
	return nil
}

// StopWallet cancels a single in-flight per-wallet run.
func (e *Engine) StopWallet(taskID, walletID int64) {
	e.mu.Lock()
	rt := e.tasks[taskID]
	e.mu.Unlock()
	if rt == nil {
		return
	}
	e.dropWalletCancel(rt, walletID)
	rt.setWallet(walletID, func(w *WalletStatus) {
		if w.Status == "running" {
			w.Status = "stopped"
		}
	})
	e.emit(rt)
}

func (e *Engine) dropWalletCancel(rt *TaskRuntime, walletID int64) {
	rt.mu.Lock()
	if rt.walletCancel != nil {
		if c := rt.walletCancel[walletID]; c != nil {
			c()
		}
		delete(rt.walletCancel, walletID)
	}
	rt.mu.Unlock()
}

func (e *Engine) runTask(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, gen int64) {
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
	// Written under rt.mu — RunWallet goroutines read/append this concurrently.
	rt.mu.Lock()
	rt.proxies = nil
	rt.mu.Unlock()
	if cfg.ProxyGroup != "" {
		if ps, perr := e.st.ListProxiesByGroup(cfg.ProxyGroup); perr == nil {
			rt.mu.Lock()
			for _, p := range ps {
				rt.proxies = append(rt.proxies, p.URL)
			}
			rt.mu.Unlock()
		}
	}

	// init statuses
	for _, lw := range wallets {
		id := lw.id
		addr := lw.addr.Hex()
		rt.setWallet(id, func(w *WalletStatus) { w.Address = addr; w.Status = "idle"; w.TxHash = ""; w.Detail = "" })
	}
	e.emit(rt)

	value := parseBigOr0(cfg.ValueWei)

	// Snipe timing: wait until StartAt before firing — for ALL modes, including Simulate
	// (which sends a real tx after its eth_call preflight, so it must also fire at open,
	// not fail preflight while the stage is still closed). Coarse wait keeps Stop
	// responsive; the send happens right when the stage opens. StartAt=0 ("Now") = fire now.
	//
	// Pre-arm: every RPC round-trip left for after the deadline is pure race latency
	// (2026-08-11: fees+gas+nonce+balance after T0 cost ~4s vs a 1s sellout). At T-15s
	// all of that runs during the wait and the tx is SIGNED, so the only network call
	// after StartAt is the broadcast itself.
	var armed []*armedTx
	if cfg.StartAt > 0 {
		for _, lw := range wallets {
			id := lw.id
			rt.setWallet(id, func(w *WalletStatus) { w.Status = "waiting"; w.Detail = "waiting for start time" })
		}
		e.emit(rt)
		e.log.Task(logger.INFO, fmt.Sprintf("waiting until %d before firing", cfg.StartAt), cfg.ID, nil)
		// Only pre-arm a genuinely FUTURE start: re-running a task whose StartAt has
		// passed must take the legacy path, where estimateGas still guards against
		// broadcasting into a closed/sold-out mint (the fallback limit would bypass it).
		if canPreArm(cfg) && time.Until(time.Unix(cfg.StartAt, 0)) > 2*time.Second {
			if waitUntil(ctx, cfg.StartAt-armLeadFor(cfg.ID)) {
				// Bound prep at T-0.5s: one hung RPC must not drag every wallet past
				// the start second; whatever misses the cut retries via the leftover
				// pass at T0.
				pctx, pcancel := context.WithDeadline(ctx, time.Unix(cfg.StartAt, 0).Add(-500*time.Millisecond))
				armed = e.preArm(pctx, rt, cfg, wallets, value)
				pcancel()
			}
		}
		if !waitUntil(ctx, cfg.StartAt) {
			// Release pre-reserved nonces: the signed txs were never broadcast, and a
			// stale +1 counter would gap this wallet's next send into a tx that can
			// never mine.
			for _, a := range armed {
				e.nonce.Invalidate(a.lw.addr)
			}
			// Superseded by a newer Start? Release the nonces (above) but don't write
			// terminal state over the live new run.
			if rt.runGen.Load() != gen {
				return
			}
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

	// Wallets that failed or missed prep still get their shot at T0 through the
	// legacy pass — a transient RPC blip 15 seconds early must not bench a wallet.
	var leftovers []loadedWallet
	if len(armed) > 0 && len(armed) < len(wallets) {
		armedSet := map[int64]bool{}
		for _, a := range armed {
			armedSet[a.lw.id] = true
		}
		for _, lw := range wallets {
			if !armedSet[lw.id] {
				leftovers = append(leftovers, lw)
			}
		}
	}

	switch {
	case cfg.Mode == ModeSimulate || cfg.Mode.isInstant():
		if len(armed) > 0 {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); e.fireArmed(ctx, rt, armed) }()
			if len(leftovers) > 0 {
				wg.Add(1)
				go func() { defer wg.Done(); e.runPass(ctx, rt, cfg, leftovers, value) }()
			}
			wg.Wait()
		} else {
			e.runPass(ctx, rt, cfg, wallets, value)
		}
	case cfg.Mode == ModeSpam:
		// Spam: every wallet loops independently (send, bump nonce, send…) so one slow
		// wallet can't hold the others back, and each keeps firing until Stop. A
		// pre-armed tx is that wallet's first shot — broadcast WITHOUT the receipt
		// watch (spam never waits on receipts; the loop itself is the retry).
		armedByWallet := map[int64]*armedTx{}
		for _, a := range armed {
			armedByWallet[a.lw.id] = a
		}
		var wg sync.WaitGroup
		for _, lw := range wallets {
			wg.Add(1)
			go func(lw loadedWallet) {
				defer wg.Done()
				wcfg, wval := cfg, value
				if cfg.hasOverride(lw.id) {
					wcfg = cfg.effective(lw.id)
					wval = parseBigOr0(wcfg.ValueWei)
				}
				if a := armedByWallet[lw.id]; a != nil {
					if res, berr := evm.Broadcast(ctx, a.tx, a.nodes, wcfg.MultiRpc); berr == nil {
						rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running"; w.TxHash = res.TxHash.Hex(); w.RPC = shortURL(res.RPC) })
						e.log.Tx(logger.INFO, "spam armed shot", wcfg.ID, lw.addr.Hex(), map[string]any{"txHash": res.TxHash.Hex(), "nonce": a.nonce})
					} else {
						e.nonce.Invalidate(lw.addr) // armed nonce never landed; loop refetches
					}
				}
				e.spamWallet(ctx, rt, wcfg, wval, lw)
			}(lw)
		}
		wg.Wait()
	default:
		// A malformed/empty Mode (hand-edited DB, raw API caller) never fires the armed
		// txs — release their reserved nonces so the wallet's next run doesn't gap.
		for _, a := range armed {
			e.nonce.Invalidate(a.lw.addr)
		}
		e.log.Task(logger.ERROR, "unknown mode "+string(cfg.Mode), cfg.ID, nil)
	}

	// A Stop→Start can have spawned a newer runner while this one was unwinding a
	// cancelled RPC. If so, this run is superseded — exit WITHOUT writing terminal
	// status/idle, or it would clobber the live new run's rows and status.
	if rt.runGen.Load() != gen {
		return
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
	// Resolve gas fees once per pass (shared across wallets, cached briefly across
	// tasks on the same chain) to avoid RPC 429 bursts when many tasks fire at the
	// same start second. On failure we log and fall back to per-wallet resolution
	// in runOne. All modes send now (Simulate = eth_call then send).
	var shared evm.ResolvedFees
	if fees, err := e.sharedFees(ctx, cfg); err != nil {
		e.log.Task(logger.WARN, "shared gas: "+err.Error()+" — per-wallet fallback", cfg.ID, nil)
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
			wcfg, wval, fees := cfg, value, shared
			if cfg.hasOverride(lw.id) { // this wallet carries its own settings (contract/gas/…)
				wcfg = cfg.effective(lw.id)
				wval = parseBigOr0(wcfg.ValueWei)
				fees = evm.ResolvedFees{} // re-resolve, the override may use different gas
			}
			e.runOne(ctx, rt, wcfg, wval, fees, lw)
		}(lw)
	}
	wg.Wait()
}

// armedTx is one wallet's fully prepared send: nodes resolved, calldata built, fees,
// gas limit, nonce, and balance all settled, and the tx SIGNED — broadcast is the
// only step left at fire time.
type armedTx struct {
	lw       loadedWallet
	cfg      TaskConfig
	nodes    []evm.Node
	tx       *types.Transaction
	nonce    uint64
	gasLimit uint64
	value    *big.Int
	data     []byte
	to       common.Address
	fees     evm.ResolvedFees
}

// armLeadSec is how many seconds before StartAt the pre-arm pass runs: late enough
// that nonce/fees stay fresh, early enough for slow RPCs to finish in time.
const armLeadSec = 15

// receiptPollTimeout bounds one receipt read so a hung endpoint releases its
// receiptSem slot instead of stalling the fleet's receipt detection.
const receiptPollTimeout = 4 * time.Second

// armSpreadSec staggers WHEN each task starts its pre-arm: task i preps at
// StartAt - (armLeadSec + id%armSpreadSec). With ~1000 tasks sharing one start
// second, waking them all at exactly T-15s fires thousands of nonce/fee/balance
// RPCs in one burst — a self-inflicted 429 storm right before the race. The spread
// smears prep across a 30s window; the fire time itself is unaffected.
const armSpreadSec = 30

// armLeadFor returns the per-task pre-arm lead (seconds before StartAt).
func armLeadFor(taskID int64) int64 {
	if taskID < 0 {
		taskID = -taskID
	}
	return armLeadSec + taskID%armSpreadSec
}

// armedFallbackGasLimit is used when pre-arm gas estimation reverts — the usual case,
// since estimateGas against a not-yet-open mint fails by design. Unused gas is
// refunded on-chain; set an explicit Gas Limit on the task to override.
const armedFallbackGasLimit = 500_000

// armedMinMintGas floors the fallback limit: a wallet whose balance can't broadcast
// at least this much gas is treated as underfunded for a mint (mint(1) ≈ 120-155k).
const armedMinMintGas = 100_000

// affordableGasLimit caps a gas limit to what `bal` can actually broadcast after
// `value`: EIP-1559 mempool admission needs bal >= value + limit*maxFee. Returns the
// largest limit <= want satisfying that (0 if the balance can't even cover value, so
// the caller's mint-gas floor benches it). Used only on the pre-arm fallback path,
// where `want` is an inflated guess and must not sign an unbroadcastable tx.
func affordableGasLimit(bal, value, maxFee *big.Int, want uint64) uint64 {
	if maxFee == nil || maxFee.Sign() <= 0 {
		return want
	}
	spendable := new(big.Int).Sub(bal, value)
	if spendable.Sign() <= 0 {
		return 0
	}
	if afford := new(big.Int).Div(spendable, maxFee); afford.IsUint64() && afford.Uint64() < want {
		return afford.Uint64()
	}
	return want
}

// canPreArm reports whether a task can be fully prepared ahead of StartAt. Only
// Flashbots can't (it targets the head block at send time). SeaDrop CAN: the
// voucher is stage-gated server-side, so pre-arm signs the on-chain mintPublic
// path instead — prepOne(early) gates on the public stage actually opening at
// StartAt, and wallets that don't qualify fall back to the voucher at T0.
func canPreArm(cfg TaskConfig) bool {
	if cfg.Flashbots {
		return false
	}
	// Only overrides for wallets actually in this task matter — stale override
	// entries for removed wallets must not silently disable pre-arm.
	want := map[int64]bool{}
	for _, id := range cfg.WalletIDs {
		want[id] = true
	}
	for id, ov := range cfg.WalletOverrides {
		if len(want) > 0 && !want[id] {
			continue
		}
		if ov.Flashbots {
			return false
		}
	}
	return true
}

// preArm runs prepOne for every wallet concurrently (mirrors runPass) and returns
// the signed txs. A wallet that fails prep is marked failed, same as a live-path
// failure; if ALL fail the caller falls back to the legacy pass at T0.
func (e *Engine) preArm(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, wallets []loadedWallet, value *big.Int) []*armedTx {
	var shared evm.ResolvedFees
	if fees, err := e.sharedFees(ctx, cfg); err != nil {
		e.log.Task(logger.WARN, "pre-arm shared gas: "+err.Error()+" — per-wallet fallback", cfg.ID, nil)
	} else {
		shared = fees
	}
	var (
		mu  sync.Mutex
		out []*armedTx
		wg  sync.WaitGroup
	)
	for _, lw := range wallets {
		wg.Add(1)
		go func(lw loadedWallet) {
			defer wg.Done()
			// Global prep throttle: with ~1000 tasks arming in the same window, an
			// unbounded fan-out melts the RPC into 429s. Bounded here — NEVER on the
			// fire path (fireArmed / the T0 leftover pass are unthrottled by design).
			select {
			case e.prepSem <- struct{}{}:
				defer func() { <-e.prepSem }()
			case <-ctx.Done():
				return
			}
			wcfg, wval, fees := cfg, value, shared
			if cfg.hasOverride(lw.id) {
				wcfg = cfg.effective(lw.id)
				wval = parseBigOr0(wcfg.ValueWei)
				fees = evm.ResolvedFees{}
			}
			if a, ok := e.prepOne(ctx, rt, wcfg, wval, fees, lw, true); ok {
				mu.Lock()
				out = append(out, a)
				mu.Unlock()
			}
		}(lw)
	}
	wg.Wait()
	e.log.Task(logger.INFO, fmt.Sprintf("pre-armed %d/%d wallets (signed; broadcast-only at start)", len(out), len(wallets)), cfg.ID, nil)
	return out
}

// spamMinIntervalMs floors the gap between two sends of one wallet when the task sets
// no DelayMs. Without a floor, a spam loop whose send fails fast (bad RPC, no funds)
// spins thousands of iterations/sec: each one logs, and every log is pushed to the UI
// over the WS, which saturates the browser event loop until the whole app is frozen and
// even Stop can't be clicked. 50ms/wallet is still ~20 sends/sec — far faster than any
// chain accepts — while keeping the UI responsive.
const spamMinIntervalMs = 50

// simulateRetryMinMs floors the gap between eth_call preflights in Simulate +
// RetryUntilSuccess, for the same reason as spamMinIntervalMs.
const simulateRetryMinMs = 250

// spamFailBackoffMs is the extra pause after a send that failed OUTRIGHT (never got a
// tx hash). Nothing recovers within microseconds, so retrying flat-out only burns CPU
// and RPC quota. ponytail: fixed backoff, no exponential — add one if a provider
// starts rate-limiting across the whole fleet.
const spamFailBackoffMs = 400

// spamWallet is Spam mode for ONE wallet: send, DON'T wait for the receipt, bump the
// nonce, send again — until the user stops the task. Unlike the other modes it never
// blocks on watchAndBump, so the wallet keeps firing at chain speed; consecutive sends
// use consecutive nonces, so each one is independently mineable.
func (e *Engine) spamWallet(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, value *big.Int, lw loadedWallet) {
	interval := time.Duration(cfg.DelayMs) * time.Millisecond
	if interval < spamMinIntervalMs*time.Millisecond {
		interval = spamMinIntervalMs * time.Millisecond
	}
	var sent, failed int
	for ctx.Err() == nil {
		start := time.Now()
		// Fees via the shared per-chain cache (2s TTL, deduped across wallets and
		// tasks) — a per-send ResolveFees would add two RPCs to every iteration.
		var shared evm.ResolvedFees
		if fees, ferr := e.sharedFees(ctx, cfg); ferr == nil {
			shared = fees
		}
		ok := e.spamOne(ctx, rt, cfg, value, shared, lw)
		if ok {
			sent++
		} else {
			failed++
		}
		if ctx.Err() != nil {
			break
		}
		rt.setWallet(lw.id, func(w *WalletStatus) {
			w.Status = "running"
			w.Detail = fmt.Sprintf("spam: %d sent · %d failed", sent, failed)
		})
		e.emit(rt)
		wait := interval - time.Since(start)
		if !ok && wait < spamFailBackoffMs*time.Millisecond {
			wait = spamFailBackoffMs * time.Millisecond // don't hot-loop on a failing send
		}
		if wait > 0 {
			sleepCtx(ctx, wait)
		}
	}
	rt.setWallet(lw.id, func(w *WalletStatus) {
		w.Status = "stopped"
		w.Detail = fmt.Sprintf("spam stopped · %d sent · %d failed", sent, failed)
	})
	e.emit(rt)
}

// spamOne prepares and broadcasts ONE spam tx, returning whether it was accepted by an
// RPC. It deliberately skips the receipt watch: the loop must keep firing, and the
// NonceManager already hands the next send the next nonce.
func (e *Engine) spamOne(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, value *big.Int, shared evm.ResolvedFees, lw loadedWallet) bool {
	a, ok := e.prepOne(ctx, rt, cfg, value, shared, lw, false)
	if !ok {
		return false
	}
	res, berr := evm.Broadcast(ctx, a.tx, a.nodes, cfg.MultiRpc)
	if berr != nil {
		msg := berr.Error()
		if evm.IsAlreadyKnown(msg) { // same tx already in the mempool — that nonce IS in flight
			return true
		}
		// Nonce rejected/stale, or the node refused it: drop the cached counter so the
		// next iteration refetches the real pending nonce instead of gapping forever.
		e.nonce.Invalidate(lw.addr)
		safe := redactErr(msg) // transport errors embed the RPC URL (+secret token)
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = "spam send failed: " + safe })
		e.log.Tx(logger.WARN, "spam send failed: "+safe, cfg.ID, lw.addr.Hex(), nil)
		return false
	}
	rt.setWallet(lw.id, func(w *WalletStatus) { w.TxHash = res.TxHash.Hex(); w.RPC = shortURL(res.RPC) })
	e.log.Tx(logger.INFO, "spam broadcast", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": res.TxHash.Hex(), "nonce": a.nonce})
	return true
}

// fireArmed broadcasts every pre-signed tx concurrently at the start second.
func (e *Engine) fireArmed(ctx context.Context, rt *TaskRuntime, armed []*armedTx) {
	var wg sync.WaitGroup
	for _, a := range armed {
		wg.Add(1)
		go func(a *armedTx) {
			defer wg.Done()
			e.fireOne(ctx, rt, a)
		}(a)
	}
	wg.Wait()
}

func (e *Engine) runOne(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, value *big.Int, shared evm.ResolvedFees, lw loadedWallet) {
	a, ok := e.prepOne(ctx, rt, cfg, value, shared, lw, false)
	if !ok {
		return
	}
	e.fireOne(ctx, rt, a)
}

// prepOne is everything up to (and including) the signature: resolve nodes, build
// calldata, fees, gas limit, nonce, balance guard, sign. With early=true it runs
// before StartAt: statuses stay "waiting", and a reverting gas estimate falls back
// to armedFallbackGasLimit instead of failing the wallet (a closed mint reverts
// estimateGas by design).
func (e *Engine) prepOne(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, value *big.Int, shared evm.ResolvedFees, lw loadedWallet, early bool) (*armedTx, bool) {
	if early {
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "waiting"; w.Detail = "arming: pre-signing tx" })
	} else {
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running"; w.Detail = "" })
	}
	e.emit(rt)

	// Early (pre-arm) failures never attempted a mint: keep them out of the activity
	// log, show Stop/deadline as "stopped" not "failed", and leave the wallet eligible
	// for the T0 leftover pass. Live-path failures keep the legacy failWallet behavior.
	fail := func(reason string) {
		if !early {
			e.failWallet(rt, lw, reason)
			return
		}
		st := "failed"
		if ctx.Err() != nil {
			st = "stopped"
		}
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = st; w.Detail = reason })
		e.log.Tx(logger.WARN, "pre-arm: "+reason, cfg.ID, lw.addr.Hex(), nil)
		e.emit(rt)
	}

	nodes, err := e.resolveNodes(ctx, cfg)
	if err != nil {
		fail("rpc: " + err.Error())
		return nil, false
	}
	client := nodes[0].Client

	to := common.HexToAddress(cfg.ContractAddress)
	var data []byte
	if cfg.Seadrop {
		qty := cfg.Quantity
		if qty < 1 {
			qty = 1
		}
		var feeOverride common.Address
		if cfg.FeeRecipient != "" {
			feeOverride = common.HexToAddress(cfg.FeeRecipient)
		}
		if early {
			// Pre-arm can't use the voucher — OpenSea only signs one once the stage is
			// open. Sign the on-chain mintPublic path instead, but ONLY when the stage
			// opening at StartAt is the public drop itself; an allowlist/FCFS snipe (or
			// a missing/ended public stage) stays unarmed and mints via the voucher in
			// the T0 leftover pass. (2026-08-12 GM drop: voucher+resolve+estimate+nonce
			// after T0 landed the fleet 3-6s past open on a 100ms-block chain.)
			r, rerr := evm.ResolveSeaDrop(ctx, client, to, qty, feeOverride)
			if rerr != nil {
				fail("seadrop pre-arm: " + rerr.Error())
				return nil, false
			}
			d := r.Drop
			if d.StartTime == 0 || int64(d.StartTime) > cfg.StartAt || (d.EndTime != 0 && int64(d.EndTime) <= cfg.StartAt) {
				rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "waiting"; w.Detail = "public stage not open at start — voucher at T0" })
				e.log.Tx(logger.INFO, "pre-arm skipped: public stage not open at StartAt", cfg.ID, lw.addr.Hex(), nil)
				e.emit(rt)
				return nil, false
			}
			to, data, value = r.To, r.Data, r.Value
			// Optional per-unit price override (editable Price/NFT in the UI).
			if cfg.MintPriceWei != "" {
				if p, ok := new(big.Int).SetString(cfg.MintPriceWei, 10); ok {
					value = new(big.Int).Mul(p, big.NewInt(int64(qty)))
				}
			}
		} else {
			// Prefer the OpenSea voucher: a server-signed mint tx that works for BOTH the
			// public AND allowlist/FCFS stages (OpenSea returns the active eligible stage
			// for this wallet). Falls back to on-chain mintPublic if the voucher is
			// unavailable (e.g. stage not open yet, or GraphQL hash rotated).
			chainSlug, _ := chains.SlugFromChainID(cfg.ChainID)
			proxyURL := ""
			rt.mu.Lock()
			if n := len(rt.proxies); n > 0 {
				proxyURL = rt.proxies[int(lw.id)%n] // rotate proxy per wallet
			}
			rt.mu.Unlock()
			v, verr := e.osc.MintVoucher(ctx, lw.addr.Hex(), to.Hex(), qty, chainSlug, proxyURL)
			if verr == nil && common.IsHexAddress(v.To) && len(common.FromHex(v.Data)) > 0 {
				to = common.HexToAddress(v.To)
				data = common.FromHex(v.Data)
				value = parseBigOr0(v.ValueWei)
			} else {
				r, rerr := evm.ResolveSeaDrop(ctx, client, to, qty, feeOverride)
				if rerr != nil {
					detail := "seadrop: " + rerr.Error()
					if verr != nil {
						detail = "seadrop voucher: " + verr.Error() + "; onchain: " + rerr.Error()
					}
					fail(detail)
					return nil, false
				}
				to, data, value = r.To, r.Data, r.Value
				// Optional per-unit price override (editable Price/NFT in the UI).
				if cfg.MintPriceWei != "" {
					if p, ok := new(big.Int).SetString(cfg.MintPriceWei, 10); ok {
						value = new(big.Int).Mul(p, big.NewInt(int64(qty)))
					}
				}
			}
		}
	} else {
		d, derr := evm.BuildCalldata(cfg.HexMode, cfg.RawHex, cfg.FunctionSig, cfg.Params, lw.addr)
		if derr != nil {
			fail("calldata: " + derr.Error())
			return nil, false
		}
		data = d
	}

	// (Simulate-mode eth_call preflight runs in fireOne — at fire time, when the
	// stage is actually open — not here, which may be before StartAt.)
	fees := shared
	if fees.MaxFeePerGas == nil {
		f, ferr := evm.ResolveFees(ctx, client, cfg.Gas)
		if ferr != nil {
			fail("gas: " + ferr.Error())
			return nil, false
		}
		fees = f
	}

	// gas limit
	var gasLimit uint64
	usedFallback := false
	if cfg.Gas.GasLimit != nil {
		gasLimit = *cfg.Gas.GasLimit
	} else if early && cfg.Seadrop {
		// A not-yet-open SeaDrop stage ALWAYS reverts estimateGas — skip the doomed
		// round-trip (200 wallets = 200 wasted RPCs + WARN spam at T-15s) and take
		// the padded fallback (balance-capped below) directly.
		gasLimit = armedFallbackGasLimit
		usedFallback = true
	} else {
		pad := evm.DefaultEstimatePad
		if cfg.Gas.EstimatePad != nil {
			pad = *cfg.Gas.EstimatePad
		}
		gl, gerr := evm.EstimateGasLimit(ctx, client, ethereum.CallMsg{From: lw.addr, To: &to, Data: data, Value: value}, pad)
		if gerr != nil {
			// A revert here is EXPECTED when the target isn't mintable yet — both for
			// pre-arm (early) and for Simulate+RetryUntilSuccess, which exists precisely
			// to sit on a closed mint. In those cases fall back to a padded limit (capped
			// to the balance below) instead of benching the wallet; the actual go/no-go
			// is the eth_call preflight in fireOne. Everything else fails loudly.
			if !early && !(cfg.Mode == ModeSimulate && cfg.RetryUntilSuccess) {
				fail("estimateGas: " + gerr.Error())
				return nil, false
			}
			gasLimit = armedFallbackGasLimit
			usedFallback = true
			e.log.Tx(logger.WARN, fmt.Sprintf("estimateGas reverted (target not open?), fallback %d: %s", gasLimit, gerr.Error()), cfg.ID, lw.addr.Hex(), nil)
		} else {
			gasLimit = gl
		}
	}

	// nonce
	var nonce uint64
	if cfg.NonceOverride != nil {
		nonce = *cfg.NonceOverride
	} else {
		n, nerr := e.nonce.Reserve(ctx, client, lw.addr)
		if nerr != nil {
			fail("nonce: " + nerr.Error())
			return nil, false
		}
		nonce = n
	}

	// Balance guard. The tx is SIGNED with gasLimit, and EIP-1559 mempool admission
	// requires balance >= value + gasLimit*maxFee — so the guard MUST cover the signed
	// cost, or a wallet admitted here gets rejected by the RPC at the T0 broadcast and
	// silently misses the mint. When estimateGas reverted at pre-arm we fell back to an
	// inflated limit; rather than bench a wallet funded for the real (smaller) mint,
	// CAP the fallback limit to what its balance can actually broadcast, down to a mint
	// floor. Unused gas is refunded on-chain, so a cap at/above the real mint cost is free.
	bal, berr := client.BalanceAt(ctx, lw.addr, nil)
	if usedFallback && berr == nil {
		gasLimit = affordableGasLimit(bal, value, fees.MaxFeePerGas, gasLimit)
	}
	gasReserve := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), fees.MaxFeePerGas)
	needed := new(big.Int).Add(new(big.Int).Set(value), gasReserve)
	if berr == nil && bal.Cmp(needed) < 0 {
		e.nonce.Invalidate(lw.addr)
		fail(fmt.Sprintf("insufficient funds: need %s wei, have %s", needed, bal))
		return nil, false
	}
	// A fallback wallet capped below a plausible mint would out-of-gas revert — bench it
	// as underfunded instead of burning gas on a guaranteed revert.
	if usedFallback && gasLimit < armedMinMintGas {
		e.nonce.Invalidate(lw.addr)
		fail(fmt.Sprintf("insufficient funds for mint gas: affordable limit %d < %d floor", gasLimit, armedMinMintGas))
		return nil, false
	}
	gasFeeEth := weiToEth(gasReserve)
	rt.setWallet(lw.id, func(w *WalletStatus) { w.GasFee = gasFeeEth })

	tx, serr := evm.SignTx(lw.key, evm.TxRequest{
		ChainID: big.NewInt(int64(cfg.ChainID)), Nonce: nonce, To: to, Data: data, Value: value,
		GasLimit: gasLimit, MaxFeePerGas: fees.MaxFeePerGas, MaxPriority: fees.MaxPriorityFeePerGas,
	})
	if serr != nil {
		e.nonce.Invalidate(lw.addr)
		fail("sign: " + serr.Error())
		return nil, false
	}
	if early {
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = fmt.Sprintf("armed: nonce %d, fires at start", nonce) })
		e.emit(rt)
	}
	return &armedTx{lw: lw, cfg: cfg, nodes: nodes, tx: tx, nonce: nonce, gasLimit: gasLimit, value: value, data: data, to: to, fees: fees}, true
}

// fireOne is the send half: Simulate-mode eth_call preflight, then broadcast with
// retry, then the receipt watch. On the armed path this is the ONLY code that runs
// after StartAt — keep RPC round-trips out of here.
func (e *Engine) fireOne(ctx context.Context, rt *TaskRuntime, a *armedTx) {
	cfg, lw := a.cfg, a.lw
	client := a.nodes[0].Client
	// Status flips to "running" but we do NOT emit here: at a 1000-wallet T0 that
	// would fire 1000 snapshot marshals into the exact race instant. The broadcast
	// result emit below carries the status; the UI is at most one broadcast late.
	rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running" })

	// Simulate mode = eth_call first, then execute only if it would succeed; on a
	// revert the wallet fails with the decoded reason and no gas is spent. (Instant
	// mode skips this check and sends straight away.) With RetryUntilSuccess the
	// preflight is retried until it passes (or the user stops) instead of failing —
	// so a task can sit on a not-yet-open mint and fire the moment it turns mintable.
	if cfg.Mode == ModeSimulate {
		for attempt := 1; ; attempt++ {
			ok, reason := evm.Simulate(ctx, client, lw.addr, a.to, a.data, a.value)
			if ok {
				break
			}
			if !cfg.RetryUntilSuccess || ctx.Err() != nil {
				e.nonce.Invalidate(lw.addr) // release the nonce reserved in prep
				if ctx.Err() != nil {
					rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "stopped"; w.Detail = "stopped while simulating" })
					e.emit(rt)
					return
				}
				e.failWallet(rt, lw, "simulate: "+reason)
				return
			}
			// Retrying: report progress, then pause. The interval is floored for the
			// same reason spam is — an un-paced eth_call loop floods the log/WS and
			// freezes the UI.
			if attempt == 1 || attempt%20 == 0 {
				rt.setWallet(lw.id, func(w *WalletStatus) {
					w.Status = "running"
					w.Detail = fmt.Sprintf("simulate retry #%d: %s", attempt, reason)
				})
				e.emit(rt)
				e.log.Tx(logger.WARN, fmt.Sprintf("simulate retry #%d: %s", attempt, reason), cfg.ID, lw.addr.Hex(), nil)
			}
			wait := time.Duration(cfg.DelayMs) * time.Millisecond
			if wait < simulateRetryMinMs*time.Millisecond {
				wait = simulateRetryMinMs * time.Millisecond
			}
			sleepCtx(ctx, wait)
			if ctx.Err() != nil {
				e.nonce.Invalidate(lw.addr)
				rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "stopped"; w.Detail = "stopped while simulating" })
				e.emit(rt)
				return
			}
		}
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = "simulate ok, sending" })
		e.log.Tx(logger.INFO, "simulate ok, sending", cfg.ID, lw.addr.Hex(), nil)
		e.emit(rt)
	}

	chainID := big.NewInt(int64(cfg.ChainID))

	// Flashbots private bundle (ETH mainnet only): never hits the public mempool, so
	// the mint can't be front-run. Falls through to public broadcast on other chains.
	if cfg.Flashbots && cfg.ChainID == 1 {
		// Apply Flashbots tuning from Settings: when the task's fees are "auto", override
		// priority/max with the configured bundle fees (builders pick by economics).
		ac := e.appConfig()
		fbFees := a.fees
		if cfg.Gas.Mode == evm.GasAuto || cfg.Gas.Mode == "" {
			if pr := numCfg(ac, "fbPriorityGwei"); pr > 0 {
				fbFees.MaxPriorityFeePerGas = gweiToWei(pr)
			}
			if mx := numCfg(ac, "fbMaxFeeGwei"); mx > 0 {
				fbFees.MaxFeePerGas = gweiToWei(mx)
			}
		}
		tx, serr := evm.SignTx(lw.key, evm.TxRequest{
			ChainID: chainID, Nonce: a.nonce, To: a.to, Data: a.data, Value: a.value,
			GasLimit: a.gasLimit, MaxFeePerGas: fbFees.MaxFeePerGas, MaxPriority: fbFees.MaxPriorityFeePerGas,
		})
		if serr != nil {
			e.nonce.Invalidate(lw.addr) // reserved in prep, never broadcast
			e.failWallet(rt, lw, "sign: "+serr.Error())
			return
		}
		head, herr := client.HeaderByNumber(ctx, nil)
		if herr != nil || head.Number == nil {
			// Without the head block we can't target a future block — a target of 0
			// would be silently accepted and never included. Fail loudly instead.
			e.nonce.Invalidate(lw.addr)
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
		e.log.Tx(logger.INFO, "flashbots bundle", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": tx.Hash().Hex(), "targetBlock": target, "nonce": a.nonce})
		e.emit(rt)
		e.watchAndBump(ctx, rt, cfg, a.nodes, lw, a.nonce, a.gasLimit, a.value, a.data, a.to, a.fees, tx.Hash())
		return
	}

	// broadcast with retry (already-known / nonce error). Attempt 0 reuses the
	// pre-signed tx — the armed hot path is broadcast-only; retries re-sign with
	// the refetched nonce.
	curNonce := a.nonce
	tx := a.tx
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			// Nothing broadcast on this nonce yet (loop entries only follow a FAILED
			// send) — release it or the wallet's next run would gap unmineably.
			e.nonce.Invalidate(lw.addr)
			rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "stopped" })
			e.emit(rt)
			return
		}
		if tx == nil {
			var serr error
			tx, serr = evm.SignTx(lw.key, evm.TxRequest{
				ChainID: chainID, Nonce: curNonce, To: a.to, Data: a.data, Value: a.value,
				GasLimit: a.gasLimit, MaxFeePerGas: a.fees.MaxFeePerGas, MaxPriority: a.fees.MaxPriorityFeePerGas,
			})
			if serr != nil {
				e.nonce.Invalidate(lw.addr)
				e.failWallet(rt, lw, "sign: "+serr.Error())
				return
			}
		}
		res, berr := evm.Broadcast(ctx, tx, a.nodes, cfg.MultiRpc)
		if berr == nil {
			rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "running"; w.TxHash = res.TxHash.Hex(); w.RPC = shortURL(res.RPC); w.Detail = "broadcast, waiting" })
			// Log host only — the RPC URL may embed a secret token.
			e.log.Tx(logger.INFO, "broadcast", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": res.TxHash.Hex(), "rpc": shortURL(res.RPC), "nonce": curNonce})
			e.emit(rt)
			e.watchAndBump(ctx, rt, cfg, a.nodes, lw, curNonce, a.gasLimit, a.value, a.data, a.to, a.fees, res.TxHash)
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
				tx = nil
				continue
			}
		}
		e.nonce.Invalidate(lw.addr)
		e.failWallet(rt, lw, "broadcast: "+redactErr(msg)) // strip any RPC token in the error
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
		// Global receipt-poll throttle: after a mass fire, thousands of wallets
		// polling every 1.5s would out-shout the broadcasts themselves. Waiting on
		// the semaphore is natural backpressure — the poll interval stretches as
		// the fleet grows. A receipt is never earlier than the next block anyway.
		select {
		case e.receiptSem <- struct{}{}:
		case <-ctx.Done():
			continue // top of loop handles ctx exit
		}
		// Per-call timeout: the pool sets no http.Client.Timeout, so a connected-but-
		// silent RPC (likely when ~1000 tasks hammer one host at T0) would otherwise
		// pin this receiptSem slot until Stop — 64 such hangs starve receipt detection
		// for the whole fleet. Bounded here so the slot always frees. Mirrors sharedFees.
		rctx, rcancel := context.WithTimeout(ctx, receiptPollTimeout)
		rcpt, rerr := client.TransactionReceipt(rctx, txHash)
		rcancel()
		<-e.receiptSem
		if rerr == nil && rcpt != nil {
			if rcpt.Status == 1 {
				rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "success"; w.Detail = fmt.Sprintf("mined block %d", rcpt.BlockNumber.Uint64()) })
				e.log.Tx(logger.INFO, "mined", cfg.ID, lw.addr.Hex(), map[string]any{"block": rcpt.BlockNumber.Uint64(), "txHash": txHash.Hex()})
				e.recordMint(cfg, lw, rcpt, value) // Home activity log + cost basis
				// Chained follow-up after a successful action (transfer minted NFT / drain ETH / ...).
				if cfg.Mode.isInstant() && cfg.PostAction != nil && cfg.PostAction.Type != "" && cfg.PostAction.Type != "none" {
					e.runPostAction(ctx, rt, cfg, nodes, lw, rcpt)
				}
			} else {
				rt.setWallet(lw.id, func(w *WalletStatus) { w.Status = "failed"; w.Detail = "reverted on-chain" })
				e.log.Tx(logger.WARN, "reverted on-chain", cfg.ID, lw.addr.Hex(), map[string]any{"txHash": txHash.Hex()})
				e.recordMintFail(cfg, lw)
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
	e.recordMintFail(rt.Config, lw)
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

// urlInErr matches an http(s) URL embedded in a string. go-ethereum transport errors
// wrap the full RPC URL, whose path can carry a secret API token.
var urlInErr = regexp.MustCompile(`https?://[^\s"']+`)

// redactErr strips any embedded RPC URL down to its host, so a secret-bearing endpoint
// never reaches the UI (WS wallet Detail) or the persisted log through an error string.
func redactErr(s string) string {
	return urlInErr.ReplaceAllStringFunc(s, shortURL)
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
// is cancelled first. Coarse 2s steps keep Stop responsive without busy-waiting; the
// FINAL step sleeps the exact remaining duration so the wake-up lands on the second
// boundary (the old whole-second arithmetic woke up to ~1s late — race-losing).
func waitUntil(ctx context.Context, unixSec int64) bool {
	target := time.Unix(unixSec, 0)
	for {
		remaining := time.Until(target)
		if remaining <= 0 {
			return true
		}
		if remaining > 2*time.Second {
			remaining = 2 * time.Second
		}
		t := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}
