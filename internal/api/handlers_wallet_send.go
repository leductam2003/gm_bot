package api

import (
	"context"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"zyperbot/internal/chains"
	"zyperbot/internal/evm"
	"zyperbot/internal/logger"
)

// nodesForChain dials all configured (or registry-default) RPCs for a chain so a send
// can broadcast (and report which endpoint accepted the tx).
func (s *Server) nodesForChain(r *http.Request, chainID int) ([]evm.Node, error) {
	return s.nodesForChainCtx(r.Context(), chainID)
}

// nodesForChainCtx is nodesForChain for background work that has no *http.Request.
func (s *Server) nodesForChainCtx(ctx context.Context, chainID int) ([]evm.Node, error) {
	var urls []string
	if es, _ := s.st.ListRPCByChain(chainID); len(es) > 0 {
		for _, e := range es {
			urls = append(urls, e.URL)
		}
	} else if c, err := chains.Get(chainID); err == nil {
		urls = append(urls, c.RPCs...)
	}
	if len(urls) == 0 {
		return nil, errors.New("no rpc for chain")
	}
	var nodes []evm.Node
	for _, u := range urls {
		if cl, err := s.pool.Dial(ctx, u); err == nil {
			nodes = append(nodes, evm.Node{URL: u, Client: cl})
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("all rpc dials failed")
	}
	return nodes, nil
}

// POST /api/wallets/{id}/send {chainId, to, amountWei, max} — send native funds from
// one wallet. Signs locally; "max" sweeps the balance minus gas.
func (s *Server) handleSendFunds(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		ChainID   int    `json:"chainId"`
		To        string `json:"to"`
		AmountWei string `json:"amountWei"`
		Max       bool   `json:"max"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !common.IsHexAddress(body.To) {
		writeErr(w, http.StatusBadRequest, "invalid recipient address")
		return
	}
	chainID := body.ChainID
	if chainID == 0 {
		chainID = 1
	}

	nodes, err := s.nodesForChain(r, chainID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	client := nodes[0].Client

	key, from, err := s.walletKey(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "wallet not found")
		return
	}
	defer wipeECDSA(key)

	ctx := r.Context()
	fees, err := evm.ResolveFees(ctx, client, evm.GasParams{Mode: evm.GasAuto})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "gas: "+err.Error())
		return
	}
	const gasLimit = uint64(21000) // exact for a plain native transfer (no calldata)
	to := common.HexToAddress(body.To)

	bal, err := client.BalanceAt(ctx, from, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "balance: "+err.Error())
		return
	}
	gasCost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), fees.MaxFeePerGas)

	var amount *big.Int
	if body.Max {
		amount = new(big.Int).Sub(bal, gasCost)
		if amount.Sign() <= 0 {
			writeErr(w, http.StatusBadRequest, "balance too low to cover gas")
			return
		}
	} else {
		a, ok := new(big.Int).SetString(strings.TrimSpace(body.AmountWei), 10)
		if !ok || a.Sign() <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid amount")
			return
		}
		amount = a
	}
	if need := new(big.Int).Add(amount, gasCost); bal.Cmp(need) < 0 {
		writeErr(w, http.StatusBadRequest, "insufficient funds: need "+need.String()+" wei, have "+bal.String())
		return
	}

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "nonce: "+err.Error())
		return
	}
	tx, err := evm.SignTx(key, evm.TxRequest{
		ChainID: big.NewInt(int64(chainID)), Nonce: nonce, To: to, Value: amount,
		GasLimit: gasLimit, MaxFeePerGas: fees.MaxFeePerGas, MaxPriority: fees.MaxPriorityFeePerGas,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sign: "+err.Error())
		return
	}
	res, err := evm.Broadcast(ctx, tx, nodes, false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "broadcast: "+err.Error())
		return
	}
	// Log host only — the RPC URL may embed a secret token; key/seed never logged.
	s.log.Tx(logger.INFO, "wallet send", 0, from.Hex(), map[string]any{
		"to": to.Hex(), "amountWei": amount.String(), "txHash": res.TxHash.Hex(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"txHash": res.TxHash.Hex(), "from": from.Hex(), "amountWei": amount.String(),
	})
}

type revealed struct {
	ID      int64  `json:"id"`
	Address string `json:"address"`
	PrivKey string `json:"privKey"`
}

// POST /api/wallets/reveal {ids:[], confirm:true} — bulk decrypt keys for "Copy
// Private Keys". Allowed ONLY from loopback (the desktop app) so plaintext keys never
// leave the local machine. Every reveal is audit-logged.
func (s *Server) handleRevealWalletsBulk(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		writeErr(w, http.StatusForbidden, "key reveal allowed only from localhost")
		return
	}
	var body struct {
		IDs     []int64 `json:"ids"`
		Confirm bool    `json:"confirm"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !body.Confirm {
		writeErr(w, http.StatusBadRequest, `must pass {"confirm":true}`)
		return
	}
	out := make([]revealed, 0, len(body.IDs))
	revealedIDs := make([]int64, 0, len(body.IDs))
	revealedAddrs := make([]string, 0, len(body.IDs))
	var skipped []int64
	for _, id := range body.IDs {
		wl, err := s.st.GetWallet(id)
		if err != nil {
			skipped = append(skipped, id)
			continue
		}
		pk, err := s.vault.Open(wl.EncPrivKey)
		if err != nil {
			skipped = append(skipped, id)
			continue
		}
		out = append(out, revealed{ID: id, Address: wl.Address, PrivKey: string(pk)})
		revealedIDs = append(revealedIDs, id)
		revealedAddrs = append(revealedAddrs, wl.Address)
		for i := range pk {
			pk[i] = 0
		}
	}
	// Name exactly which keys were disclosed (and which were requested-but-skipped) so
	// the audit trail can answer "which keys were compromised" after an incident.
	s.log.API(logger.WARN, "PRIVATE KEYS REVEALED (bulk)", map[string]any{
		"count": len(out), "walletIds": revealedIDs, "addresses": revealedAddrs,
		"skipped": skipped, "remote": r.RemoteAddr,
	})
	writeJSON(w, http.StatusOK, out)
}

// isLoopback reports whether the request originated from the local machine.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
