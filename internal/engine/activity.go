package engine

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"zyperbot/internal/store"
)

// recordMint persists a successful on-chain mint for the Home dashboard: the activity log
// and the cost basis (mint value + gas paid) used for realized PNL. Best-effort — a store
// error must never affect the mint flow. Simulate runs send nothing, so they're skipped.
func (e *Engine) recordMint(cfg TaskConfig, lw loadedWallet, rcpt *types.Receipt, value *big.Int) {
	if e.st == nil || cfg.Mode == ModeSimulate {
		return
	}
	cost := new(big.Int)
	if value != nil {
		cost.Set(value)
	}
	tx := ""
	if rcpt != nil {
		tx = rcpt.TxHash.Hex()
		if rcpt.EffectiveGasPrice != nil {
			cost.Add(cost, new(big.Int).Mul(new(big.Int).SetUint64(rcpt.GasUsed), rcpt.EffectiveGasPrice))
		}
	}
	tokenID := ""
	if id := findMintedTokenID(rcpt, lw.addr); id != nil {
		tokenID = id.String()
	}
	_, _ = e.st.AddMint(store.Mint{
		Ts: time.Now(), ChainID: cfg.ChainID, Contract: cfg.ContractAddress, TokenID: tokenID,
		WalletID: lw.id, Address: lw.addr.Hex(), TxHash: tx, CostWei: cost.String(), Status: "minted",
	})
}

// recordMintFail records a failed mint attempt (so the Home "N failed" counter is real).
func (e *Engine) recordMintFail(cfg TaskConfig, lw loadedWallet) {
	if e.st == nil || cfg.Mode == ModeSimulate {
		return
	}
	_, _ = e.st.AddMint(store.Mint{
		Ts: time.Now(), ChainID: cfg.ChainID, Contract: cfg.ContractAddress,
		WalletID: lw.id, Address: lw.addr.Hex(), Status: "failed", CostWei: "0",
	})
}
