package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"zyperbot/internal/config"
)

var abiClient = &http.Client{Timeout: 12 * time.Second}

// fetchABIJSON pulls a verified contract's ABI JSON from Etherscan V2 (multichain).
func fetchABIJSON(ctx context.Context, chainID int, address string) (string, error) {
	key := config.EtherscanKey()
	if key == "" {
		return "", errors.New("no ETHERSCAN_API_KEY")
	}
	if chainID == 0 {
		chainID = 1
	}
	q := url.Values{}
	q.Set("chainid", strconv.Itoa(chainID))
	q.Set("module", "contract")
	q.Set("action", "getabi")
	q.Set("address", common.HexToAddress(address).Hex())
	q.Set("apikey", key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.etherscan.io/v2/api?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := abiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var er struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(raw, &er) != nil || er.Status != "1" {
		msg := er.Result
		if msg == "" {
			msg = er.Message
		}
		if msg == "" {
			msg = "ABI not available (is the contract verified?)"
		}
		return "", errors.New(msg)
	}
	return er.Result, nil
}

// POST /api/contract/abi {chainId, address} — fetch a verified contract's ABI.
func (s *Server) handleFetchABI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID int    `json:"chainId"`
		Address string `json:"address"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !common.IsHexAddress(body.Address) {
		writeErr(w, http.StatusBadRequest, "invalid contract address")
		return
	}
	if config.EtherscanKey() == "" {
		writeErr(w, http.StatusBadRequest, "ABI fetch needs ETHERSCAN_API_KEY in .env — or paste the ABI")
		return
	}
	abiJSON, err := fetchABIJSON(r.Context(), body.ChainID, body.Address)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "explorer: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"abi": abiJSON})
}

// POST /api/contract/tx {chainId, hash} — "replay" a transaction: pull its target
// contract, value, and (if the contract is verified) decode the function + params so
// the same call can be fired from every selected wallet. The original sender's address
// in the args is rewritten to {address} so each wallet substitutes its own.
func (s *Server) handleTxReplay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID int    `json:"chainId"`
		Hash    string `json:"hash"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	hash := strings.TrimSpace(body.Hash)
	if !isTxHash(hash) {
		writeErr(w, http.StatusBadRequest, "invalid tx hash")
		return
	}
	chainID := body.ChainID
	if chainID == 0 {
		chainID = 1
	}
	client, err := s.clientForChain(r, chainID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	tx, _, err := client.TransactionByHash(r.Context(), common.HexToHash(hash))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "tx not found on this chain — pick the right Chain: "+err.Error())
		return
	}
	if tx.To() == nil {
		writeErr(w, http.StatusBadRequest, "that tx is a contract creation (no target to replay)")
		return
	}
	to := *tx.To()
	input := tx.Data()
	// On a chain mismatch (wrong Chain selected) Sender returns the zero address with
	// an error — capture it so we never (a) skip rewriting the real recipient, nor
	// (b) mistake a legitimate address(0) arg for the sender and redirect the call.
	from, fromErr := types.Sender(types.LatestSignerForChainID(big.NewInt(int64(chainID))), tx)

	out := map[string]any{
		"contractAddress": to.Hex(),
		"valueWei":        tx.Value().String(),
		"chainId":         chainID,
		"rawInput":        hexutil.Encode(input),
		"from":            from.Hex(),
	}
	// Decode to a readable function + params when the contract is verified AND all arg
	// types are simple enough to re-encode (BuildCalldata can't rebuild arrays/tuples).
	if len(input) >= 4 {
		if abiJSON, e := fetchABIJSON(r.Context(), chainID, to.Hex()); e == nil {
			if parsed, pe := abi.JSON(strings.NewReader(abiJSON)); pe == nil {
				if method, me := parsed.MethodById(input[:4]); me == nil {
					if args, ue := method.Inputs.Unpack(input[4:]); ue == nil && simpleArgs(method.Inputs) && fromErr == nil && from != (common.Address{}) {
						params := make([]string, len(args))
						for i, a := range args {
							params[i] = formatArg(method.Inputs[i].Type, a, from)
						}
						out["functionSig"] = method.Sig
						out["params"] = params
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func isTxHash(s string) bool {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// simpleArgs reports whether every input is a type BuildCalldata can re-encode.
func simpleArgs(args abi.Arguments) bool {
	for _, a := range args {
		switch a.Type.T {
		// String excluded on purpose: the UI transports params as a ';'-joined field
		// and trims each, which would corrupt a string containing ';' or significant
		// whitespace. Functions with string args fall back to exact raw-Hex replay.
		case abi.AddressTy, abi.BoolTy, abi.UintTy, abi.IntTy, abi.BytesTy, abi.FixedBytesTy:
		default:
			return false
		}
	}
	return true
}

// formatArg turns a decoded ABI value into the string BuildCalldata expects, rewriting
// the original sender's address to {address} so the replay is per-wallet.
func formatArg(t abi.Type, v interface{}, from common.Address) string {
	switch t.T {
	case abi.AddressTy:
		if a, ok := v.(common.Address); ok {
			if from != (common.Address{}) && a == from {
				return "{address}"
			}
			return a.Hex()
		}
	case abi.BoolTy:
		return fmt.Sprintf("%t", v)
	case abi.StringTy:
		if s, ok := v.(string); ok {
			return s
		}
	case abi.BytesTy:
		if b, ok := v.([]byte); ok {
			return hexutil.Encode(b)
		}
	case abi.FixedBytesTy:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Array {
			b := make([]byte, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				b[i] = byte(rv.Index(i).Uint())
			}
			return hexutil.Encode(b)
		}
	}
	return fmt.Sprintf("%v", v) // uint/int (decimal) + any fallback
}
