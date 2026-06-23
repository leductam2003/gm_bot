// Package engine is the concurrent task runner: each task runs in its own goroutine,
// fans out across its wallets with a bounded worker pool, and is controllable via
// Start/Stop/Boost. It ports the StartTask/SendSingleTx behavior of zyper-mac's
// mintEngine for the generic (non-OpenSea) task case — Simulate / Spam / Action.
package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"zyperbot/internal/evm"
)

// Mode is the task execution mode (the Simulate/Spam/Action toggle in the UI).
type Mode string

const (
	ModeSimulate Mode = "simulate" // eth_call only, no gas spent
	ModeSpam     Mode = "spam"     // repeat Action until stopped
	ModeAction   Mode = "action"   // send once per wallet
)

// TaskConfig is the persisted task definition (stored as JSON).
type TaskConfig struct {
	ID              int64         `json:"id"`
	Group           string        `json:"group"`
	ChainID         int           `json:"chainId"`
	ContractAddress string        `json:"contractAddress"`
	Mode            Mode          `json:"mode"`
	HexMode         bool          `json:"hexMode"`
	FunctionSig     string        `json:"functionSig"`     // e.g. "mint(uint256,address)"
	RawHex          string        `json:"rawHex"`          // used when HexMode
	Params          []string      `json:"params"`          // may contain {address}
	ValueWei        string        `json:"valueWei"`        // decimal wei string
	// SeaDrop (OpenSea) public mint: when true, calldata is built as
	// SeaDrop.mintPublic(contract, feeRecipient, 0x0, quantity) and value = price*qty
	// is read on-chain — overriding FunctionSig/RawHex/ValueWei.
	Seadrop         bool          `json:"seadrop"`
	Quantity        int           `json:"quantity"`        // units per wallet for seadrop
	FeeRecipient    string        `json:"feeRecipient"`    // optional override
	MintPriceWei    string        `json:"mintPriceWei"`    // optional per-unit price override (wei) for on-chain seadrop
	StartAt         int64         `json:"startAt"`         // unix seconds: wait until this time before firing (0 = now)
	WalletIDs       []int64       `json:"walletIds"`       // empty = all evm wallets
	RPCUrls         []string      `json:"rpcUrls"`         // empty = chain default/group
	ProxyGroup      string        `json:"proxyGroup"`      // route OpenSea poll/voucher through this proxy group (rotated per wallet)
	MultiRpc        bool          `json:"multiRpc"`
	Gas             evm.GasParams `json:"gas"`
	NonceOverride   *uint64       `json:"nonceOverride,omitempty"`
	DelayMs         int           `json:"delayMs"`
	SpamGuardrailMs int           `json:"spamGuardrailMs"`
	Preflight       bool          `json:"preflight"` // action: eth_call before send
	Flashbots       bool          `json:"flashbots"`   // ETH mainnet: send via private bundle (anti-frontrun)
	PostAction      *PostAction   `json:"postAction,omitempty"` // chained follow-up after a successful action
}

// PostAction is a follow-up performed after the main action (mint) succeeds on a wallet.
type PostAction struct {
	Type        string  `json:"type"`        // none | transfer | list | accept | drain
	Destination string  `json:"destination"` // recipient (transfer) / proceeds (drain)
	DrainETH    bool    `json:"drainEth"`    // also sweep leftover ETH to Destination
	FeeGwei     float64 `json:"feeGwei"`     // optional priority-fee override for the follow-up
	PriceWei    string  `json:"priceWei"`    // listing price (list)
}

// WalletStatus is the per-wallet runtime state shown in the task table.
type WalletStatus struct {
	WalletID int64  `json:"walletId"`
	Address  string `json:"address"`
	Status   string `json:"status"` // idle|running|success|failed|stopped|skipped
	TxHash   string `json:"txHash,omitempty"`
	RPC      string `json:"rpc,omitempty"`
	GasFee   string `json:"gasFee,omitempty"` // estimated, ETH
	Detail   string `json:"detail,omitempty"`
}

// TaskRuntime wraps a config with live status + cancellation.
type TaskRuntime struct {
	mu           sync.Mutex
	Config       TaskConfig
	Status       string // idle|running|stopped|done
	Wallets      map[int64]*WalletStatus
	proxies      []string                     // resolved once per run from Config.ProxyGroup; rotated per wallet
	cancel       context.CancelFunc           // whole-task run
	walletCancel map[int64]context.CancelFunc // per-wallet runs (one row's ▶ button)
	pumpEpoch    atomic.Int64
}

func newRuntime(cfg TaskConfig) *TaskRuntime {
	return &TaskRuntime{Config: cfg, Status: "idle", Wallets: map[int64]*WalletStatus{}}
}

// snapshot is the JSON shape pushed to the UI (over WS and REST).
type snapshot struct {
	ID        int64           `json:"id"`
	Group     string          `json:"group"`
	Mode      Mode            `json:"mode"`
	Status    string          `json:"status"`
	ChainID   int             `json:"chainId"`
	WalletIDs []int64         `json:"walletIds"` // which wallets this task targets (empty = all evm)
	Seadrop   bool            `json:"seadrop"`
	ValueWei  string          `json:"valueWei"`
	Wallets   []*WalletStatus `json:"wallets"`
}

func (rt *TaskRuntime) snapshot() snapshot {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ws := make([]*WalletStatus, 0, len(rt.Wallets))
	for _, w := range rt.Wallets {
		cp := *w
		ws = append(ws, &cp)
	}
	return snapshot{
		ID: rt.Config.ID, Group: rt.Config.Group, Mode: rt.Config.Mode,
		Status: rt.Status, ChainID: rt.Config.ChainID,
		WalletIDs: rt.Config.WalletIDs, Seadrop: rt.Config.Seadrop, ValueWei: rt.Config.ValueWei,
		Wallets: ws,
	}
}

func (rt *TaskRuntime) setWallet(id int64, mutate func(*WalletStatus)) {
	rt.mu.Lock()
	w := rt.Wallets[id]
	if w == nil {
		w = &WalletStatus{WalletID: id, Status: "idle"}
		rt.Wallets[id] = w
	}
	mutate(w)
	rt.mu.Unlock()
}

func (rt *TaskRuntime) setStatus(s string) {
	rt.mu.Lock()
	rt.Status = s
	rt.mu.Unlock()
}
