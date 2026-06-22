package evm

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// FlashbotsRelay is the default builder endpoint (ETH mainnet). Override via env if needed.
const FlashbotsRelay = "https://relay.flashbots.net"

var fbClient = &http.Client{Timeout: 10 * time.Second}

// flashbotsSignature builds the X-Flashbots-Signature header: the signer address, a
// colon, and an EIP-191 personal_sign over the hex keccak256 of the JSON body. The
// signing key only identifies the searcher (reputation); it never holds funds, so an
// ephemeral key is fine for basic inclusion.
func flashbotsSignature(body []byte, key *ecdsa.PrivateKey) (string, error) {
	hash := gethcrypto.Keccak256Hash(body)
	msg := []byte("0x" + hex.EncodeToString(hash.Bytes()))
	sig, err := gethcrypto.Sign(accounts.TextHash(msg), key)
	if err != nil {
		return "", err
	}
	sig[64] += 27 // V: 0/1 -> 27/28
	addr := gethcrypto.PubkeyToAddress(key.PublicKey)
	return addr.Hex() + ":0x" + hex.EncodeToString(sig), nil
}

// SendFlashbotsBundle submits a single signed tx as a private bundle targeting one
// block. Returns nil if the relay accepted it (no public mempool exposure).
func SendFlashbotsBundle(ctx context.Context, signedTx *types.Transaction, targetBlock uint64, signKey *ecdsa.PrivateKey) error {
	raw, err := signedTx.MarshalBinary()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_sendBundle",
		"params": []any{map[string]any{
			"txs":         []string{"0x" + hex.EncodeToString(raw)},
			"blockNumber": hexutil.EncodeUint64(targetBlock),
		}},
	}
	body, _ := json.Marshal(payload)
	sig, err := flashbotsSignature(body, signKey)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, FlashbotsRelay, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Flashbots-Signature", sig)
	resp, err := fbClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		s := string(rb)
		if len(s) > 200 {
			s = s[:200]
		}
		return fmt.Errorf("flashbots %d: %s", resp.StatusCode, s)
	}
	var r struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rb, &r)
	if r.Error != nil {
		return fmt.Errorf("flashbots: %s", r.Error.Message)
	}
	return nil
}

// SubmitFlashbots signs an ephemeral searcher key and submits the bundle for the next
// `blocks` blocks (relays only include in the targeted block, so resubmitting widens
// the inclusion window). Returns the tx hash to poll for a receipt.
func SubmitFlashbots(ctx context.Context, signedTx *types.Transaction, nextBlock uint64, blocks int) error {
	if blocks < 1 {
		blocks = 1
	}
	ephKey, err := gethcrypto.GenerateKey()
	if err != nil {
		return err
	}
	var lastErr error
	sent := false
	for b := nextBlock; b < nextBlock+uint64(blocks); b++ {
		if err := SendFlashbotsBundle(ctx, signedTx, b, ephKey); err != nil {
			lastErr = err
			continue
		}
		sent = true
	}
	if !sent {
		return lastErr
	}
	return nil
}
