package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"zyperbot/internal/chains"
	"zyperbot/internal/crypto"
	"zyperbot/internal/events"
	"zyperbot/internal/evm"
	"zyperbot/internal/logger"
	"zyperbot/internal/opensea"
	"zyperbot/internal/rpc"
	"zyperbot/internal/store"
)

var ErrNotFound = errors.New("task not found")
var ErrRunning = errors.New("task already running")

// Engine owns all task runtimes and their concurrency.
type Engine struct {
	st    *store.Store
	vault *crypto.Vault
	pool  *rpc.Pool
	log   *logger.Logger
	hub   *events.Hub
	nonce *evm.NonceManager
	osc   *opensea.Client

	mu    sync.Mutex
	tasks map[int64]*TaskRuntime

	feeMu    sync.Mutex
	feeCache map[string]*feeEntry // per-chain auto-fee cache (see sharedFees)

	// prepSem bounds concurrent pre-arm RPC chains across ALL tasks; receiptSem
	// bounds concurrent receipt polls. Neither ever gates the fire path.
	prepSem    chan struct{}
	receiptSem chan struct{}
	// voucherSem bounds concurrent OpenSea voucher fetches across ALL wallets. OpenSea
	// rate-limits vouchers ~4/window/IP and answers a limit as HTTP 200 "Too Many
	// Requests"; a 200-wallet fleet firing at once self-429s. Capping the burst (with
	// per-wallet proxy rotation) keeps it under the limit. Pre-signed armed wallets
	// don't fetch at fire time, so this never gates the FCFS broadcast.
	voucherSem chan struct{}

	// cacheMu guards the brief DB-read caches below — at a 1000-task fire second,
	// per-wallet SQLite reads would serialize the hot path on the single connection.
	cacheMu  sync.Mutex
	urlCache map[int]urlCacheEntry
	appCfg   map[string]any
	appCfgAt time.Time
}

type urlCacheEntry struct {
	urls []string
	at   time.Time
}

const (
	urlCacheTTL = 5 * time.Second // RPC-page / Settings edits propagate within this
	cfgCacheTTL = 5 * time.Second
)

func New(st *store.Store, vault *crypto.Vault, pool *rpc.Pool, log *logger.Logger, hub *events.Hub) *Engine {
	return &Engine{
		st: st, vault: vault, pool: pool, log: log, hub: hub,
		nonce:      evm.NewNonceManager(),
		osc:        opensea.New(),
		tasks:      map[int64]*TaskRuntime{},
		feeCache:   map[string]*feeEntry{},
		prepSem:    make(chan struct{}, 256), // ~4 RPCs/wallet ≈ bounded, race-safe prep throughput
		receiptSem: make(chan struct{}, 64),
		voucherSem: make(chan struct{}, 12), // keep the OpenSea voucher burst under the rate limit
		urlCache:   map[int]urlCacheEntry{},
	}
}

// feeEntry caches one chain's resolved auto fees briefly. When hundreds of tasks
// share a start time they all wake the same second; without this each would fire
// its own baseFee + tip lookups (2 RPC calls × N tasks in one burst).
type feeEntry struct {
	sem  chan struct{} // 1-slot resolver token; waiters select on it AND their ctx
	mu   sync.Mutex    // guards fees/at only (never held across RPC)
	fees evm.ResolvedFees
	at   time.Time
}

const (
	feeCacheTTL = 2 * time.Second
	// feeResolveTimeout bounds one resolver's RPC time so a hung endpoint can
	// never stall the queue of same-key tasks behind it for long.
	feeResolveTimeout = 5 * time.Second
)

// sharedFees resolves gas fees for a task, deduplicating concurrent lookups per
// (chain, endpoint, gas-params) key and reusing a fresh-enough result. Manual gas
// needs no RPC and bypasses the cache. Waiting is context-aware: Stop on a queued
// task returns immediately instead of sitting in a mutex.
func (e *Engine) sharedFees(ctx context.Context, cfg TaskConfig) (evm.ResolvedFees, error) {
	nodes, err := e.resolveNodes(ctx, cfg)
	if err != nil {
		return evm.ResolvedFees{}, err
	}
	g := cfg.Gas
	if g.Mode == evm.GasManual {
		return evm.ResolveFees(ctx, nodes[0].Client, g)
	}
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	// The endpoint is part of the key so a task pinned to a broken custom RPC
	// shares nothing with tasks resolving through a healthy one.
	key := fmt.Sprintf("%d|%s|%.3f|%.3f|%t", cfg.ChainID, nodes[0].URL, f(g.AutoMultiplier), f(g.MinPriorityGwei), g.PinPriority)
	e.feeMu.Lock()
	ent := e.feeCache[key]
	if ent == nil {
		ent = &feeEntry{sem: make(chan struct{}, 1)}
		e.feeCache[key] = ent
	}
	e.feeMu.Unlock()

	cached := func() (evm.ResolvedFees, bool) {
		ent.mu.Lock()
		defer ent.mu.Unlock()
		if ent.fees.MaxFeePerGas != nil && time.Since(ent.at) < feeCacheTTL {
			return ent.fees, true
		}
		return evm.ResolvedFees{}, false
	}
	if fees, ok := cached(); ok {
		return fees, nil
	}
	// Acquire the single resolver slot — or bail the moment the task is stopped.
	select {
	case ent.sem <- struct{}{}:
	case <-ctx.Done():
		return evm.ResolvedFees{}, ctx.Err()
	}
	defer func() { <-ent.sem }()
	// The previous holder may have refreshed the cache while we queued.
	if fees, ok := cached(); ok {
		return fees, nil
	}
	// Freshness is stamped from BEFORE the lookup: ResolveFees reads the baseFee
	// header first, so dating the entry from completion would let a slow node
	// serve fees older than the TTL promises.
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, feeResolveTimeout)
	defer cancel()
	fees, err := evm.ResolveFees(rctx, nodes[0].Client, g)
	if err != nil {
		return evm.ResolvedFees{}, err
	}
	ent.mu.Lock()
	ent.fees, ent.at = fees, start
	ent.mu.Unlock()
	return fees, nil
}

// Load reads persisted tasks into memory (status idle).
func (e *Engine) Load() error {
	rows, err := e.st.ListTasks()
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rows {
		var cfg TaskConfig
		if err := json.Unmarshal([]byte(r.ConfigJSON), &cfg); err != nil {
			continue
		}
		cfg.ID = r.ID
		cfg.Group = r.GroupName
		e.tasks[r.ID] = newRuntime(cfg)
	}
	return nil
}

// Create persists a new task and registers its runtime.
func (e *Engine) Create(cfg TaskConfig) (int64, error) {
	blob, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	id, err := e.st.AddTask(cfg.Group, string(blob))
	if err != nil {
		return 0, err
	}
	cfg.ID = id
	e.mu.Lock()
	e.tasks[id] = newRuntime(cfg)
	e.mu.Unlock()
	e.log.Task(logger.INFO, "task created", id, map[string]any{"chain": cfg.ChainID, "mode": cfg.Mode})
	return id, nil
}

// GetConfig returns a task's full stored config (for the edit form).
func (e *Engine) GetConfig(id int64) (TaskConfig, error) {
	e.mu.Lock()
	rt := e.tasks[id]
	e.mu.Unlock()
	if rt == nil {
		return TaskConfig{}, ErrNotFound
	}
	rt.mu.Lock()
	cfg := rt.Config
	rt.mu.Unlock()
	return cfg, nil
}

// Update replaces a task config (must not be running).
func (e *Engine) Update(id int64, cfg TaskConfig) error {
	e.mu.Lock()
	rt := e.tasks[id]
	e.mu.Unlock()
	if rt == nil {
		return ErrNotFound
	}
	rt.mu.Lock()
	running := rt.Status == "running"
	rt.mu.Unlock()
	if running {
		return ErrRunning
	}
	cfg.ID = id
	blob, _ := json.Marshal(cfg)
	if err := e.st.UpdateTaskConfig(id, cfg.Group, string(blob)); err != nil {
		return err
	}
	rt.mu.Lock()
	rt.Config = cfg
	rt.Wallets = map[int64]*WalletStatus{}
	rt.mu.Unlock()
	return nil
}

// Delete stops (if running) and removes a task.
func (e *Engine) Delete(id int64) error {
	e.Stop(id)
	e.mu.Lock()
	delete(e.tasks, id)
	e.mu.Unlock()
	return e.st.DeleteTask(id)
}

// TaskSummary is a flat, public summary of a task for non-UI consumers (Telegram).
type TaskSummary struct {
	ID      int64
	Group   string
	Mode    Mode
	Status  string
	ChainID int
	Total   int
	Success int
	Failed  int
	Running int
}

// Summaries returns a compact summary of every task.
func (e *Engine) Summaries() []TaskSummary {
	out := []TaskSummary{}
	for _, s := range e.List() {
		sum := TaskSummary{ID: s.ID, Group: s.Group, Mode: s.Mode, Status: s.Status, ChainID: s.ChainID, Total: len(s.Wallets)}
		for _, w := range s.Wallets {
			switch w.Status {
			case "success":
				sum.Success++
			case "failed":
				sum.Failed++
			case "running":
				sum.Running++
			}
		}
		out = append(out, sum)
	}
	return out
}

// List returns snapshots of every task for the UI.
func (e *Engine) List() []snapshot {
	e.mu.Lock()
	rts := make([]*TaskRuntime, 0, len(e.tasks))
	for _, rt := range e.tasks {
		rts = append(rts, rt)
	}
	e.mu.Unlock()
	out := make([]snapshot, 0, len(rts))
	for _, rt := range rts {
		out = append(out, rt.snapshot())
	}
	return out
}

// Stop cancels a running task, including any per-wallet (▶) runs, and immediately
// publishes the stopping state. The runner goroutine may still be inside an RPC call
// when this returns; without the eager status flip the UI would keep showing "running"
// (with Start still refusing as ErrRunning) until that call unwound.
func (e *Engine) Stop(id int64) {
	e.mu.Lock()
	rt := e.tasks[id]
	e.mu.Unlock()
	if rt == nil {
		return
	}
	rt.mu.Lock()
	cancel := rt.cancel
	rt.cancel = nil
	// Per-wallet ▶ runs have their own contexts — a task-level Stop must cancel those
	// too, or a spam-looping wallet keeps firing after the user stopped the task.
	walletCancels := make([]context.CancelFunc, 0, len(rt.walletCancel))
	for wid, c := range rt.walletCancel {
		if c != nil {
			walletCancels = append(walletCancels, c)
		}
		delete(rt.walletCancel, wid)
	}
	if rt.Status == "running" {
		rt.Status = "stopping"
	}
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, c := range walletCancels {
		c()
	}
	e.emit(rt)
}

// Boost bumps the pump epoch — running action/spam tasks re-broadcast pending txs
// with higher gas (port of the ⚡ Pump button in mintEngine.ts).
func (e *Engine) Boost(id int64) error {
	e.mu.Lock()
	rt := e.tasks[id]
	e.mu.Unlock()
	if rt == nil {
		return ErrNotFound
	}
	rt.pumpEpoch.Add(1)
	e.log.Task(logger.INFO, "boost requested (pump gas)", id, nil)
	return nil
}

// groupIDs collects the task IDs in a group without marshaling snapshots — at 1000
// tasks, e.List() would serialize every wallet of every task just to read the group.
func (e *Engine) groupIDs(group string) []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []int64
	for id, rt := range e.tasks {
		// Lock order e.mu -> rt.mu matches Start(); Config is written under rt.mu.
		rt.mu.Lock()
		g := rt.Config.Group
		rt.mu.Unlock()
		if g == group {
			ids = append(ids, id)
		}
	}
	return ids
}

// StartGroup starts every task in a group; StopGroup stops them.
func (e *Engine) StartGroup(group string) {
	for _, id := range e.groupIDs(group) {
		_ = e.Start(id)
	}
}
func (e *Engine) StopGroup(group string) {
	for _, s := range e.List() {
		if s.Group == group {
			e.Stop(s.ID)
		}
	}
}

// resolveNodes turns a task's RPC config into dialed clients (explicit URLs, then
// the chain's configured group, then the registry default).
func (e *Engine) resolveNodes(ctx context.Context, cfg TaskConfig) ([]evm.Node, error) {
	urls := cfg.RPCUrls
	if len(urls) == 0 {
		urls = e.chainURLs(cfg.ChainID)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("no RPC for chain %d", cfg.ChainID)
	}
	var nodes []evm.Node
	for _, u := range urls {
		cl, err := e.pool.Dial(ctx, u)
		if err != nil {
			// Log host only — the URL may embed a secret token.
			e.log.API(logger.WARN, "rpc dial failed", map[string]any{"host": shortURL(u), "err": err.Error()})
			continue
		}
		nodes = append(nodes, evm.Node{URL: u, Client: cl})
	}
	if len(nodes) == 0 {
		return nil, errors.New("all RPC dials failed")
	}
	return nodes, nil
}

// emitMinGap coalesces per-task snapshot pushes. Every wallet state flip used to
// publish a full task snapshot; with a 1000-wallet task that's O(wallets²) JSON
// work in one pass and floods the WS. At most one snapshot per gap is sent, plus
// a guaranteed trailing one carrying the final state.
const emitMinGap = 150 * time.Millisecond

func (e *Engine) emit(rt *TaskRuntime) {
	rt.emitMu.Lock()
	if rt.emitTimer != nil { // a trailing emit is already scheduled; it will pick up this change
		rt.emitMu.Unlock()
		return
	}
	wait := emitMinGap - time.Since(rt.lastEmit)
	if wait <= 0 {
		rt.lastEmit = time.Now()
		rt.emitMu.Unlock()
		e.hub.Publish("task", rt.snapshot())
		return
	}
	rt.emitTimer = time.AfterFunc(wait, func() {
		rt.emitMu.Lock()
		rt.lastEmit = time.Now()
		rt.emitTimer = nil
		rt.emitMu.Unlock()
		e.hub.Publish("task", rt.snapshot())
	})
	rt.emitMu.Unlock()
}

// chainURLs resolves the DB/registry-derived RPC list for a chain, cached briefly —
// per-wallet SQLite reads inside the fire path would serialize a 1000-wallet fleet
// on the store's single connection. Explicit task RPCUrls bypass this entirely.
func (e *Engine) chainURLs(chainID int) []string {
	e.cacheMu.Lock()
	if ent, ok := e.urlCache[chainID]; ok && time.Since(ent.at) < urlCacheTTL {
		e.cacheMu.Unlock()
		return ent.urls
	}
	e.cacheMu.Unlock()
	var urls []string
	if es, err := e.st.ListRPCByChain(chainID); err == nil {
		for _, ep := range es {
			urls = append(urls, ep.URL)
		}
	}
	if len(urls) == 0 {
		// per-chain RPC override from Settings (Chains card) before the registry default
		if ov := chainRPCOverride(e.appConfig(), chainID); ov != "" {
			urls = []string{ov}
		}
	}
	if len(urls) == 0 {
		if c, err := chains.Get(chainID); err == nil {
			urls = c.RPCs
		}
	}
	e.cacheMu.Lock()
	e.urlCache[chainID] = urlCacheEntry{urls: urls, at: time.Now()}
	e.cacheMu.Unlock()
	return urls
}

// appConfig reads the dashboard's app.config JSON blob (Flashbots tuning, chain RPC
// overrides, …), cached briefly for the same single-SQLite-connection reason as
// chainURLs. Returns an empty map if unset. Treat the returned map as read-only.
func (e *Engine) appConfig() map[string]any {
	e.cacheMu.Lock()
	if e.appCfg != nil && time.Since(e.appCfgAt) < cfgCacheTTL {
		m := e.appCfg
		e.cacheMu.Unlock()
		return m
	}
	e.cacheMu.Unlock()
	m := map[string]any{}
	if v, err := e.st.GetSetting("app.config"); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &m)
	}
	e.cacheMu.Lock()
	e.appCfg, e.appCfgAt = m, time.Now()
	e.cacheMu.Unlock()
	return m
}

// openSeaSlug resolves the OpenSea chain slug for chainID for the voucher path: the
// registry slug, else a user-defined custom chain's name normalized (Robinhood ->
// "robinhood"). Returns "" if unknown — callers must NOT fall back to a wrong chain
// (e.g. ethereum), which queries a DIFFERENT drop and OpenSea answers DropNotFound, so
// the allowlist/public voucher always fails. Mirrors the API's openSeaChainSlug.
func (e *Engine) openSeaSlug(chainID int) string { return openSeaSlugFrom(chainID, e.appConfig()) }

func openSeaSlugFrom(chainID int, appCfg map[string]any) string {
	if sl, err := chains.SlugFromChainID(chainID); err == nil && sl != "" {
		return sl
	}
	ccs, _ := appCfg["customChains"].([]any)
	for _, it := range ccs {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["id"].(float64); int(id) == chainID {
			if name, _ := m["name"].(string); strings.TrimSpace(name) != "" {
				return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", ""))
			}
		}
	}
	return ""
}

func numCfg(m map[string]any, k string) float64 {
	if v, ok := m[k]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

func gweiToWei(g float64) *big.Int { return big.NewInt(int64(g * 1e9)) }

// chainRPCOverride returns the user's per-chain fallback RPC from Settings ("" if none).
func chainRPCOverride(m map[string]any, chainID int) string {
	ov, ok := m["chainRPCOverrides"].(map[string]any)
	if !ok {
		return ""
	}
	if u, ok := ov[fmt.Sprint(chainID)].(string); ok {
		return u
	}
	return ""
}
