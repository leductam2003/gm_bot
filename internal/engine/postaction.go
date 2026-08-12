package engine

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"

	"zyperbot/internal/evm"
	"zyperbot/internal/logger"
)

// runPostAction performs the chained follow-up after a successful action (mint) on a
// wallet: transfer the freshly-minted NFT, drain leftover ETH, etc. Best-effort — a
// failure here is logged and surfaced in the wallet detail but doesn't fail the mint.
func (e *Engine) runPostAction(ctx context.Context, rt *TaskRuntime, cfg TaskConfig, nodes []evm.Node, lw loadedWallet, rcpt *types.Receipt) {
	pa := cfg.PostAction
	client := nodes[0].Client
	chainID := big.NewInt(int64(cfg.ChainID))

	fees, ferr := evm.ResolveFees(ctx, client, cfg.Gas)
	if ferr != nil {
		e.log.Tx(logger.WARN, "post-action gas: "+ferr.Error(), cfg.ID, lw.addr.Hex(), nil)
		return
	}
	if pa.FeeGwei > 0 {
		prio := big.NewInt(int64(pa.FeeGwei * 1e9))
		fees.MaxPriorityFeePerGas = prio
		if fees.MaxFeePerGas.Cmp(prio) < 0 {
			fees.MaxFeePerGas = new(big.Int).Mul(prio, big.NewInt(2))
		}
	}

	// Capture the next nonce ONCE and advance it locally per send, so chained
	// follow-ups (transfer then drain) are deterministically ordered — re-polling the
	// node's pending nonce isn't reliable right after a broadcast (esp. multi-RPC).
	nonce, nerr := client.PendingNonceAt(ctx, lw.addr)
	if nerr != nil {
		e.log.Tx(logger.WARN, "post-action nonce: "+nerr.Error(), cfg.ID, lw.addr.Hex(), nil)
		return
	}
	// Gas the earlier sends in this chain reserve but haven't spent yet: they are still
	// pending (we never wait for a receipt), so a later drain that sizes itself from the
	// on-chain balance must leave room for them, or geth rejects the queued drain
	// (cumulative cost of all pending nonces > balance) and the ETH is stranded.
	pendingGas := big.NewInt(0)
	send := func(to common.Address, data []byte, value *big.Int, gasLimit uint64, label string) bool {
		tx, serr := evm.SignTx(lw.key, evm.TxRequest{
			ChainID: chainID, Nonce: nonce, To: to, Data: data, Value: value,
			GasLimit: gasLimit, MaxFeePerGas: fees.MaxFeePerGas, MaxPriority: fees.MaxPriorityFeePerGas,
		})
		if serr != nil {
			e.log.Tx(logger.WARN, "post-action sign: "+serr.Error(), cfg.ID, lw.addr.Hex(), nil)
			return false
		}
		res, berr := evm.Broadcast(ctx, tx, nodes, cfg.MultiRpc)
		if berr != nil {
			e.log.Tx(logger.WARN, "post-action "+label+": "+berr.Error(), cfg.ID, lw.addr.Hex(), nil)
			rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = "mint ok · " + label + " failed" })
			e.emit(rt)
			return false
		}
		nonce++ // advance for the next chained follow-up (e.g. drain after transfer)
		// This tx is now pending on nonce N; reserve its worst-case gas for any later
		// drain that sizes off the balance.
		pendingGas.Add(pendingGas, new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), fees.MaxFeePerGas))
		e.log.Tx(logger.INFO, "post-action "+label, cfg.ID, lw.addr.Hex(), map[string]any{"to": to.Hex(), "txHash": res.TxHash.Hex()})
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = "mint ok · " + label + " sent" })
		e.emit(rt)
		return true
	}

	drain := func() {
		if !common.IsHexAddress(pa.Destination) {
			e.log.Tx(logger.WARN, "post-action drain: invalid destination", cfg.ID, lw.addr.Hex(), nil)
			return
		}
		dest := common.HexToAddress(pa.Destination)
		bal, berr := client.BalanceAt(ctx, lw.addr, nil)
		if berr != nil {
			return
		}
		// Size the native drain from the node — some custom chains need more than the
		// 21000 intrinsic floor and reject it with "intrinsic gas too low".
		gasLimit := evm.NativeGasLimit(ctx, client, lw.addr, dest, big.NewInt(0))
		gasCost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), fees.MaxFeePerGas)
		// Leave room for THIS drain's gas AND any still-pending earlier send (e.g. the
		// NFT transfer we just broadcast) — else the queued drain exceeds the balance.
		amt := new(big.Int).Sub(bal, new(big.Int).Add(gasCost, pendingGas))
		if amt.Sign() <= 0 {
			e.log.Tx(logger.WARN, "post-action drain: balance below gas, nothing to drain", cfg.ID, lw.addr.Hex(), nil)
			return
		}
		send(dest, nil, amt, gasLimit, "drain ETH")
	}

	switch pa.Type {
	case "transfer":
		if !common.IsHexAddress(pa.Destination) {
			e.log.Tx(logger.WARN, "post-action transfer: invalid destination", cfg.ID, lw.addr.Hex(), nil)
			return
		}
		tokenID := findMintedTokenID(rcpt, lw.addr)
		if tokenID == nil {
			e.log.Tx(logger.WARN, "post-action transfer: no minted tokenId found in receipt", cfg.ID, lw.addr.Hex(), nil)
			return
		}
		nft := common.HexToAddress(cfg.ContractAddress)
		dest := common.HexToAddress(pa.Destination)
		data := erc721TransferCalldata(lw.addr, dest, tokenID)
		gl, gerr := evm.EstimateGasLimit(ctx, client, ethereum.CallMsg{From: lw.addr, To: &nft, Data: data}, evm.DefaultEstimatePad)
		if gerr != nil {
			gl = 120_000
		}
		send(nft, data, big.NewInt(0), gl, "transfer NFT #"+tokenID.String())
		if pa.DrainETH {
			drain()
		}
	case "drain":
		drain()
	case "list", "accept":
		// Seaport listing / offer acceptance needs price + signed order params that
		// aren't in this inline form — surface it instead of silently doing nothing.
		e.log.Tx(logger.WARN, "post-action "+pa.Type+" not yet wired — list/accept via the NFT tab", cfg.ID, lw.addr.Hex(), nil)
		rt.setWallet(lw.id, func(w *WalletStatus) { w.Detail = "mint ok · " + pa.Type + " pending (use NFT tab)" })
		e.emit(rt)
	}
}

// findMintedTokenID scans a receipt for the ERC721 Transfer(_, wallet, tokenId) event
// (the mint), returning the tokenId minted to the wallet (nil if none).
func findMintedTokenID(rcpt *types.Receipt, wallet common.Address) *big.Int {
	sig := gethcrypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	for _, lg := range rcpt.Logs {
		if len(lg.Topics) == 4 && lg.Topics[0] == sig {
			to := common.BytesToAddress(lg.Topics[2].Bytes())
			if to == wallet {
				return new(big.Int).SetBytes(lg.Topics[3].Bytes())
			}
		}
	}
	return nil
}

// erc721TransferCalldata builds transferFrom(from,to,tokenId) calldata.
func erc721TransferCalldata(from, to common.Address, tokenID *big.Int) []byte {
	sel := gethcrypto.Keccak256([]byte("transferFrom(address,address,uint256)"))[:4]
	out := make([]byte, 0, 4+96)
	out = append(out, sel...)
	out = append(out, common.LeftPadBytes(from.Bytes(), 32)...)
	out = append(out, common.LeftPadBytes(to.Bytes(), 32)...)
	out = append(out, common.LeftPadBytes(tokenID.Bytes(), 32)...)
	return out
}
