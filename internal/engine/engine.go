package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"

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
}

func New(st *store.Store, vault *crypto.Vault, pool *rpc.Pool, log *logger.Logger, hub *events.Hub) *Engine {
	return &Engine{
		st: st, vault: vault, pool: pool, log: log, hub: hub,
		nonce: evm.NewNonceManager(),
		osc:   opensea.New(),
		tasks: map[int64]*TaskRuntime{},
	}
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

// Stop cancels a running task.
func (e *Engine) Stop(id int64) {
	e.mu.Lock()
	rt := e.tasks[id]
	e.mu.Unlock()
	if rt == nil {
		return
	}
	rt.mu.Lock()
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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

// StartGroup starts every task in a group; StopGroup stops them.
func (e *Engine) StartGroup(group string) {
	for _, s := range e.List() {
		if s.Group == group {
			_ = e.Start(s.ID)
		}
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
		if es, err := e.st.ListRPCByChain(cfg.ChainID); err == nil {
			for _, ep := range es {
				urls = append(urls, ep.URL)
			}
		}
	}
	if len(urls) == 0 {
		// per-chain RPC override from Settings (Chains card) before the registry default
		if ov := chainRPCOverride(e.appConfig(), cfg.ChainID); ov != "" {
			urls = []string{ov}
		}
	}
	if len(urls) == 0 {
		c, err := chains.Get(cfg.ChainID)
		if err != nil {
			return nil, err
		}
		urls = c.RPCs
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

func (e *Engine) emit(rt *TaskRuntime) { e.hub.Publish("task", rt.snapshot()) }

// appConfig reads the dashboard's app.config JSON blob (Flashbots tuning, chain RPC
// overrides, …). Returns an empty map if unset.
func (e *Engine) appConfig() map[string]any {
	m := map[string]any{}
	if v, err := e.st.GetSetting("app.config"); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &m)
	}
	return m
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
