package evm

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Node pairs an RPC URL with its dialed client (so broadcast can report which
// endpoint accepted the tx).
type Node struct {
	URL    string
	Client *ethclient.Client
}

// TxRequest is everything needed to sign one EIP-1559 transaction.
type TxRequest struct {
	ChainID      *big.Int
	Nonce        uint64
	To           common.Address
	Data         []byte
	Value        *big.Int
	GasLimit     uint64
	MaxFeePerGas *big.Int
	MaxPriority  *big.Int
}

// SignTx signs locally (no RPC) — the port of account.signTransaction in mintEngine.ts.
func SignTx(key *ecdsa.PrivateKey, r TxRequest) (*types.Transaction, error) {
	val := r.Value
	if val == nil {
		val = big.NewInt(0)
	}
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   r.ChainID,
		Nonce:     r.Nonce,
		GasTipCap: r.MaxPriority,
		GasFeeCap: r.MaxFeePerGas,
		Gas:       r.GasLimit,
		To:        &r.To,
		Value:     val,
		Data:      r.Data,
	})
	signer := types.LatestSignerForChainID(r.ChainID)
	return types.SignTx(tx, signer, key)
}

// BroadcastResult reports the accepted tx hash and which RPC accepted it.
type BroadcastResult struct {
	TxHash common.Hash
	RPC    string
}

// Broadcast sends a signed tx. With multiRpc it fires at every node and returns the
// first acceptance (Promise.any semantics from mintEngine.ts broadcast()).
func Broadcast(ctx context.Context, tx *types.Transaction, nodes []Node, multiRpc bool) (BroadcastResult, error) {
	if len(nodes) == 0 {
		return BroadcastResult{}, errors.New("no RPC nodes to broadcast to")
	}
	targets := nodes
	if !multiRpc {
		targets = nodes[:1]
	}
	if len(targets) == 1 {
		if err := targets[0].Client.SendTransaction(ctx, tx); err != nil {
			return BroadcastResult{}, err
		}
		return BroadcastResult{TxHash: tx.Hash(), RPC: targets[0].URL}, nil
	}

	type res struct {
		url string
		err error
	}
	ch := make(chan res, len(targets))
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, 16) // cap concurrent sends even if many RPCs are configured
	for _, n := range targets {
		go func(n Node) {
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- res{url: n.URL, err: n.Client.SendTransaction(cctx, tx)}
		}(n)
	}
	var firstErr error
	for i := 0; i < len(targets); i++ {
		r := <-ch
		if r.err == nil {
			return BroadcastResult{TxHash: tx.Hash(), RPC: r.url}, nil
		}
		if firstErr == nil {
			firstErr = r.err
		}
	}
	return BroadcastResult{}, firstErr
}

// Simulate does an eth_call (no gas) and returns a decoded revert reason, or "" if
// the call would succeed. Port of mintEngine.ts preflight().
func Simulate(ctx context.Context, c *ethclient.Client, from, to common.Address, data []byte, value *big.Int) (ok bool, reason string) {
	msg := ethereum.CallMsg{From: from, To: &to, Data: data, Value: value}
	ret, err := c.CallContract(ctx, msg, nil)
	if err == nil {
		return true, ""
	}
	// go-ethereum surfaces revert data on some clients via the error; try to decode.
	if de, okk := err.(rpcDataError); okk {
		if r := DecodeRevert(de.ErrorData()); r != "" {
			return false, r
		}
	}
	if r := DecodeRevert(ret); r != "" {
		return false, r
	}
	return false, err.Error()
}

// rpcDataError is implemented by go-ethereum's json-rpc error carrying revert data.
type rpcDataError interface{ ErrorData() []byte }

// WaitReceipt polls for the receipt until mined or ctx is done.
func WaitReceipt(ctx context.Context, c *ethclient.Client, hash common.Hash, pollEvery time.Duration) (*types.Receipt, error) {
	if pollEvery <= 0 {
		pollEvery = 1500 * time.Millisecond
	}
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		rcpt, err := c.TransactionReceipt(ctx, hash)
		if err == nil {
			return rcpt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
