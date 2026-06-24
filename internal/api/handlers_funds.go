package api

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"zyperbot/internal/evm"
	"zyperbot/internal/logger"
)

// fundsReq is the Manage-Funds request body (disperse or consolidate).
type fundsReq struct {
	Mode          string  `json:"mode"`
	ChainID       int     `json:"chainId"`
	Token         string  `json:"token"`
	RunID         string  `json:"runId"`
	FromWalletID  int64   `json:"fromWalletId"`  // disperse: funder
	ToWalletIDs   []int64 `json:"toWalletIds"`   // disperse: recipients
	FromWalletIDs []int64 `json:"fromWalletIds"` // consolidate: sources
	ToAddress     string  `json:"toAddress"`     // consolidate: destination
	Max           bool    `json:"max"`           // consolidate: sweep balance
	AmountEth     string  `json:"amountEth"`     // per-wallet amount (human units)
}

// fundsRun carries the resolved, validated context shared by both modes.
type fundsRun struct {
	body      fundsReq
	nodes     []evm.Node
	fees      evm.ResolvedFees
	tokenAddr common.Address
	isToken   bool
	decimals  int
	total     int
}

// fundResult is one streamed Manage-Funds transfer outcome (WS "funds" event).
type fundResult struct {
	RunID     string `json:"runId"`
	Total     int    `json:"total"`
	Index     int    `json:"index"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	AmountWei string `json:"amountWei,omitempty"`
	TxHash    string `json:"txHash,omitempty"`
	Error     string `json:"error,omitempty"`
	Fatal     bool   `json:"fatal,omitempty"` // whole run aborted (e.g. funder underfunded)
}

// POST /api/funds/move — disperse (1 funder -> N wallets) or consolidate (N wallets ->
// 1 address), native or ERC-20. Validates synchronously, returns {runId,total}, then
// streams each transfer over the WS ("funds" event) because a large batch can outlast
// the 30s HTTP timeout. Keys are decrypted per-transfer and wiped immediately.
func (s *Server) handleFundsMove(w http.ResponseWriter, r *http.Request) {
	var body fundsReq
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if body.Mode != "disperse" && body.Mode != "consolidate" {
		writeErr(w, http.StatusBadRequest, "mode must be disperse or consolidate")
		return
	}
	chainID := body.ChainID
	if chainID == 0 {
		chainID = 1
	}
	body.ChainID = chainID

	var tokenAddr common.Address
	isToken := false
	if t := strings.TrimSpace(body.Token); t != "" {
		if !common.IsHexAddress(t) {
			writeErr(w, http.StatusBadRequest, "invalid token address")
			return
		}
		tokenAddr = common.HexToAddress(t)
		isToken = true
	}

	nodes, err := s.nodesForChain(r, chainID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	client := nodes[0].Client
	fees, err := evm.ResolveFees(r.Context(), client, evm.GasParams{Mode: evm.GasAuto})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "gas: "+err.Error())
		return
	}
	decimals := 18
	if isToken {
		d, derr := evm.ERC20Decimals(r.Context(), client, tokenAddr)
		if derr != nil {
			writeErr(w, http.StatusBadGateway, "token decimals: "+derr.Error())
			return
		}
		decimals = d
	}

	var total int
	switch body.Mode {
	case "disperse":
		if body.FromWalletID == 0 {
			writeErr(w, http.StatusBadRequest, "pick a funder wallet")
			return
		}
		if len(body.ToWalletIDs) == 0 {
			writeErr(w, http.StatusBadRequest, "pick at least one recipient")
			return
		}
		if _, perr := parseUnits(body.AmountEth, decimals); perr != nil {
			writeErr(w, http.StatusBadRequest, "amount: "+perr.Error())
			return
		}
		total = len(body.ToWalletIDs)
	case "consolidate":
		if len(body.FromWalletIDs) == 0 {
			writeErr(w, http.StatusBadRequest, "pick at least one source wallet")
			return
		}
		if !common.IsHexAddress(body.ToAddress) {
			writeErr(w, http.StatusBadRequest, "invalid destination address")
			return
		}
		if !body.Max {
			if _, perr := parseUnits(body.AmountEth, decimals); perr != nil {
				writeErr(w, http.StatusBadRequest, "amount: "+perr.Error())
				return
			}
		}
		total = len(body.FromWalletIDs)
	}

	writeJSON(w, http.StatusOK, map[string]any{"runId": body.RunID, "total": total})

	run := fundsRun{body: body, nodes: nodes, fees: fees, tokenAddr: tokenAddr, isToken: isToken, decimals: decimals, total: total}
	// Detach from the request: the batch may outlive it (r.Context() cancels on response).
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if body.RunID != "" { // allow POST /funds/cancel to stop the remaining legs
			s.fundCancels.Store(body.RunID, cancel)
			defer s.fundCancels.Delete(body.RunID)
		}
		if body.Mode == "disperse" {
			s.disperse(ctx, run)
		} else {
			s.consolidate(ctx, run)
		}
	}()
}

// POST /api/funds/cancel {runId} — halt an in-flight run. Legs already handed to the
// broadcaster may still confirm on-chain; this only stops the legs not yet started.
func (s *Server) handleFundsCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunID string `json:"runId"`
	}
	_ = decode(r, &body)
	if v, ok := s.fundCancels.Load(body.RunID); ok {
		if cancel, ok2 := v.(context.CancelFunc); ok2 {
			cancel()
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// disperse sends `amount` from one funder to each recipient, signing sequentially with a
// locally-incremented nonce. Funds are checked up front so it won't half-drain the funder.
func (s *Server) disperse(ctx context.Context, run fundsRun) {
	b := run.body
	client := run.nodes[0].Client
	pub := s.fundsPublisher(b.RunID, run.total)

	key, from, err := s.walletKey(b.FromWalletID)
	if err != nil {
		pub(fundResult{Fatal: true, Error: "funder: " + err.Error()})
		return
	}
	defer wipeECDSA(key)

	amount, _ := parseUnits(b.AmountEth, run.decimals) // validated in the handler
	n := int64(run.total)

	// Gas: native is exact (21000); ERC-20 is estimated per-recipient in the loop below.
	// repGas is a conservative ceiling used only for the upfront funder reserve.
	repGas := uint64(21000)
	if run.isToken {
		repGas = 100000 // safe reserve even if the sample estimate fails
		if wl, e := s.st.GetWallet(b.ToWalletIDs[0]); e == nil {
			if data, de := evm.ERC20TransferData(common.HexToAddress(wl.Address), amount); de == nil {
				if g, ge := evm.EstimateGas(ctx, client, from, run.tokenAddr, data, big.NewInt(0)); ge == nil {
					if bg := gasWithBuffer(g, nil); bg > repGas {
						repGas = bg
					}
				}
			}
		}
	}
	gasCost := new(big.Int).Mul(new(big.Int).SetUint64(repGas), run.fees.MaxFeePerGas)

	// Upfront funding check — abort before sending anything if the funder can't cover all.
	if run.isToken {
		tokBal, e := evm.ERC20BalanceOf(ctx, client, run.tokenAddr, from)
		if e != nil {
			pub(fundResult{Fatal: true, Error: "funder token balance: " + e.Error()})
			return
		}
		if need := new(big.Int).Mul(amount, big.NewInt(n)); tokBal.Cmp(need) < 0 {
			pub(fundResult{Fatal: true, Error: "funder token too low: need " + need.String() + ", have " + tokBal.String()})
			return
		}
		natBal, e := client.BalanceAt(ctx, from, nil)
		if e != nil {
			pub(fundResult{Fatal: true, Error: "funder balance: " + e.Error()})
			return
		}
		if need := new(big.Int).Mul(gasCost, big.NewInt(n)); natBal.Cmp(need) < 0 {
			pub(fundResult{Fatal: true, Error: "funder needs native gas ~" + need.String() + " wei, have " + natBal.String()})
			return
		}
	} else {
		bal, e := client.BalanceAt(ctx, from, nil)
		if e != nil {
			pub(fundResult{Fatal: true, Error: "funder balance: " + e.Error()})
			return
		}
		perLeg := new(big.Int).Add(amount, gasCost)
		if need := new(big.Int).Mul(perLeg, big.NewInt(n)); bal.Cmp(need) < 0 {
			pub(fundResult{Fatal: true, Error: "funder balance too low: need " + need.String() + " wei, have " + bal.String()})
			return
		}
	}

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		pub(fundResult{Fatal: true, Error: "nonce: " + err.Error()})
		return
	}

	for i, wid := range b.ToWalletIDs {
		if ctx.Err() != nil {
			return
		}
		wl, gerr := s.st.GetWallet(wid)
		if gerr != nil {
			pub(fundResult{Index: i, From: from.Hex(), Error: "recipient wallet not found"})
			continue
		}
		recipient := common.HexToAddress(wl.Address)
		txTo, value := recipient, amount
		var data []byte
		legGas := uint64(21000)
		if run.isToken {
			d, de := evm.ERC20TransferData(recipient, amount)
			if de != nil {
				pub(fundResult{Index: i, From: from.Hex(), To: recipient.Hex(), Error: "encode: " + de.Error()})
				continue
			}
			data, txTo, value = d, run.tokenAddr, big.NewInt(0)
			// Estimate per recipient — a transfer to a zero-balance address costs more gas
			// (cold storage), so a single shared estimate can underprice later legs.
			g, ge := evm.EstimateGas(ctx, client, from, run.tokenAddr, data, big.NewInt(0))
			legGas = gasWithBuffer(g, ge) // floors at 90000, never the 21000 native default
		}
		hash, serr := s.signAndBroadcast(ctx, key, b.ChainID, nonce, txTo, value, data, legGas, run.fees, run.nodes)
		res := fundResult{Index: i, From: from.Hex(), To: recipient.Hex(), AmountWei: amount.String()}
		if serr != nil {
			res.Error = serr.Error()
			// Ambiguous failure — the tx may or may not have consumed the nonce. Resync
			// from the node so a possibly-used nonce doesn't cascade-fail the whole tail.
			if n2, e2 := client.PendingNonceAt(ctx, from); e2 == nil {
				nonce = n2
			}
		} else {
			res.TxHash = hash
			nonce++ // advance only on an accepted broadcast
			s.log.Tx(logger.INFO, "disperse", 0, from.Hex(), map[string]any{"to": recipient.Hex(), "amountWei": amount.String(), "txHash": hash})
		}
		pub(res)
	}
}

// consolidate sweeps each source wallet into one destination address. Each source signs
// with its own nonce; native sweeps leave a gas reserve, ERC-20 sweeps move the full
// token balance (the source must still hold native ETH to pay gas).
func (s *Server) consolidate(ctx context.Context, run fundsRun) {
	b := run.body
	client := run.nodes[0].Client
	pub := s.fundsPublisher(b.RunID, run.total)
	dest := common.HexToAddress(b.ToAddress)

	var fixedAmount *big.Int
	if !b.Max {
		fixedAmount, _ = parseUnits(b.AmountEth, run.decimals) // validated in the handler
	}

	for i, wid := range b.FromWalletIDs {
		if ctx.Err() != nil {
			return
		}
		func() {
			key, from, err := s.walletKey(wid)
			if err != nil {
				pub(fundResult{Index: i, Error: "wallet not found"})
				return
			}
			defer wipeECDSA(key)

			var txTo common.Address
			var value, moved *big.Int
			var data []byte
			var gasLimit uint64

			if run.isToken {
				tokBal, e := evm.ERC20BalanceOf(ctx, client, run.tokenAddr, from)
				if e != nil {
					pub(fundResult{Index: i, From: from.Hex(), Error: "token balance: " + e.Error()})
					return
				}
				amount := tokBal
				if !b.Max {
					amount = fixedAmount
					if tokBal.Cmp(amount) < 0 {
						pub(fundResult{Index: i, From: from.Hex(), Error: "insufficient token: have " + tokBal.String()})
						return
					}
				}
				if amount.Sign() <= 0 {
					pub(fundResult{Index: i, From: from.Hex(), Error: "zero token balance"})
					return
				}
				d, de := evm.ERC20TransferData(dest, amount)
				if de != nil {
					pub(fundResult{Index: i, From: from.Hex(), Error: "encode: " + de.Error()})
					return
				}
				g, ge := evm.EstimateGas(ctx, client, from, run.tokenAddr, d, big.NewInt(0))
				gasLimit = gasWithBuffer(g, ge)
				gasCost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), run.fees.MaxFeePerGas)
				natBal, e := client.BalanceAt(ctx, from, nil)
				if e != nil {
					pub(fundResult{Index: i, From: from.Hex(), Error: "balance: " + e.Error()})
					return
				}
				if natBal.Cmp(gasCost) < 0 {
					pub(fundResult{Index: i, From: from.Hex(), Error: "no native ETH for gas to move token"})
					return
				}
				txTo, value, data, moved = run.tokenAddr, big.NewInt(0), d, amount
			} else {
				bal, e := client.BalanceAt(ctx, from, nil)
				if e != nil {
					pub(fundResult{Index: i, From: from.Hex(), Error: "balance: " + e.Error()})
					return
				}
				gasLimit = 21000
				gasCost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), run.fees.MaxFeePerGas)
				var amount *big.Int
				if b.Max {
					amount = new(big.Int).Sub(bal, gasCost)
					if amount.Sign() <= 0 {
						pub(fundResult{Index: i, From: from.Hex(), Error: "balance below gas cost"})
						return
					}
				} else {
					amount = fixedAmount
					if need := new(big.Int).Add(amount, gasCost); bal.Cmp(need) < 0 {
						pub(fundResult{Index: i, From: from.Hex(), Error: "insufficient: need " + need.String() + " wei, have " + bal.String()})
						return
					}
				}
				txTo, value, moved = dest, amount, amount
			}

			nonce, err := client.PendingNonceAt(ctx, from)
			if err != nil {
				pub(fundResult{Index: i, From: from.Hex(), Error: "nonce: " + err.Error()})
				return
			}
			hash, serr := s.signAndBroadcast(ctx, key, b.ChainID, nonce, txTo, value, data, gasLimit, run.fees, run.nodes)
			res := fundResult{Index: i, From: from.Hex(), To: dest.Hex(), AmountWei: moved.String()}
			if serr != nil {
				res.Error = serr.Error()
			} else {
				res.TxHash = hash
				s.log.Tx(logger.INFO, "consolidate", 0, from.Hex(), map[string]any{"to": dest.Hex(), "amountWei": moved.String(), "txHash": hash})
			}
			pub(res)
		}()
	}
}

// signAndBroadcast signs one EIP-1559 tx and broadcasts it. The tx hash is deterministic
// from the signed tx, so it is returned even on a broadcast error. An "already known"
// reply means the node already has this tx in its mempool — that consumed the nonce, so
// it is reported as success (err=nil) to keep a sequential-nonce batch from cascading.
func (s *Server) signAndBroadcast(ctx context.Context, key *ecdsa.PrivateKey, chainID int, nonce uint64, to common.Address, value *big.Int, data []byte, gasLimit uint64, fees evm.ResolvedFees, nodes []evm.Node) (string, error) {
	tx, err := evm.SignTx(key, evm.TxRequest{
		ChainID: big.NewInt(int64(chainID)), Nonce: nonce, To: to, Value: value, Data: data,
		GasLimit: gasLimit, MaxFeePerGas: fees.MaxFeePerGas, MaxPriority: fees.MaxPriorityFeePerGas,
	})
	if err != nil {
		return "", err
	}
	hash := tx.Hash().Hex()
	if _, berr := evm.Broadcast(ctx, tx, nodes, false); berr != nil {
		if evm.IsAlreadyKnown(berr.Error()) {
			return hash, nil // already in the mempool — nonce consumed, treat as sent
		}
		return hash, berr
	}
	return hash, nil
}

func (s *Server) fundsPublisher(runID string, total int) func(fundResult) {
	return func(res fundResult) {
		res.RunID = runID
		res.Total = total
		if res.Error != "" { // every funds error (validation, fatal, broadcast) lands in the Logs tab
			lvl := logger.WARN
			if res.Fatal {
				lvl = logger.ERROR
			}
			s.log.API(lvl, "funds transfer error", map[string]any{"from": res.From, "to": res.To, "error": res.Error, "fatal": res.Fatal})
		}
		s.hub.Publish("funds", res)
	}
}

// gasWithBuffer pads an estimate by 25% (ERC-20 transfer cost varies); falls back to a
// safe ceiling when the estimate is unavailable. Unused gas is refunded on-chain.
func gasWithBuffer(g uint64, err error) uint64 {
	if err != nil || g == 0 {
		return 90000
	}
	return g + g/4
}

// parseUnits converts a human decimal string ("0.01") to integer base units for the
// given decimals (18 for native ETH). Strict grammar: ASCII digits with at most one
// dot — no sign, no exponent, no stray characters. Fractional precision beyond
// `decimals` is rejected if it would drop non-zero digits (silent under-send), so the
// amount the user typed is exactly what gets signed.
func parseUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty amount")
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if !isDigits(intPart) || !isDigits(frac) {
		return nil, errors.New("invalid number (digits only, e.g. 0.05)")
	}
	if len(frac) > decimals {
		if strings.Trim(frac[decimals:], "0") != "" {
			return nil, fmt.Errorf("too many decimal places (max %d for this token)", decimals)
		}
		frac = frac[:decimals]
	}
	for len(frac) < decimals {
		frac += "0"
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		digits = "0"
	}
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, errors.New("invalid number")
	}
	if v.Sign() <= 0 {
		return nil, errors.New("must be greater than zero")
	}
	return v, nil
}

// isDigits reports whether s is empty or all ASCII digits (no sign, no '.', no 'e').
func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
