package evm

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Seaport 1.6 + OpenSea conduit constants (verbatim from zyper-mac/src/listing.ts).
const (
	Seaport16     = "0x0000000000000068F116a894984e2DB1123eB395"
	OSConduitKey  = "0x0000007b02230091a7ed01230072f7006a004d60a8d4e71d599b8104250f0000"
	OSConduit     = "0x1E0049783F008A0085193E00003D00cd54003c71"
	zeroBytes32   = "0x0000000000000000000000000000000000000000000000000000000000000000"
)

var seaportABI = mustABI(`[
	{"type":"function","name":"getCounter","stateMutability":"view","inputs":[{"name":"offerer","type":"address"}],"outputs":[{"type":"uint256"}]},
	{"type":"function","name":"incrementCounter","stateMutability":"nonpayable","inputs":[],"outputs":[]}
]`)

var erc721ApprovalABI = mustABI(`[
	{"type":"function","name":"isApprovedForAll","stateMutability":"view","inputs":[{"name":"owner","type":"address"},{"name":"operator","type":"address"}],"outputs":[{"type":"bool"}]},
	{"type":"function","name":"setApprovalForAll","stateMutability":"nonpayable","inputs":[{"name":"operator","type":"address"},{"name":"approved","type":"bool"}],"outputs":[]}
]`)

// SeaportCounter reads the offerer's current Seaport counter (usually 0).
func SeaportCounter(ctx context.Context, c *ethclient.Client, offerer common.Address) (*big.Int, error) {
	sp := common.HexToAddress(Seaport16)
	vals, err := callView(ctx, c, sp, seaportABI, "getCounter", offerer)
	if err != nil {
		return nil, err
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("bad counter")
	}
	return n, nil
}

// IsApprovedForConduit reports whether owner approved the OpenSea conduit on nft.
func IsApprovedForConduit(ctx context.Context, c *ethclient.Client, nft, owner common.Address) (bool, error) {
	vals, err := callView(ctx, c, nft, erc721ApprovalABI, "isApprovedForAll", owner, common.HexToAddress(OSConduit))
	if err != nil {
		return false, err
	}
	b, ok := vals[0].(bool)
	return ok && b, nil
}

// BuildSetApprovalForAll returns calldata to approve the OpenSea conduit on nft.
func BuildSetApprovalForAll() []byte {
	data, _ := erc721ApprovalABI.Pack("setApprovalForAll", common.HexToAddress(OSConduit), true)
	return data
}

// BuildIncrementCounter returns Seaport.incrementCounter() calldata (cancels all the
// caller's open orders under the current counter).
func BuildIncrementCounter() []byte {
	data, _ := seaportABI.Pack("incrementCounter")
	return data
}

// SeaportAddress returns the Seaport 1.6 address.
func SeaportAddress() common.Address { return common.HexToAddress(Seaport16) }

// Fee is a marketplace/creator fee for a collection.
type Fee struct {
	Recipient string
	Bps       int64 // basis points (250 = 2.5%)
}

// Listing is the signed order ready to POST to OpenSea.
type Listing struct {
	Parameters map[string]interface{} `json:"parameters"`
	Signature  string                 `json:"signature"`
	Protocol   string                 `json:"protocol_address"`
}

// BuildAndSignListing constructs a Seaport listing order (sell 1 ERC721 for ETH),
// signs it EIP-712 with the owner key, and returns the OpenSea-ready payload.
// Pure given (counter, fees, now, salt) — port of zyper-mac/src/listing.ts buildOrder.
func BuildAndSignListing(key *ecdsa.PrivateKey, chainID int, counter *big.Int,
	nft common.Address, tokenID, priceWei *big.Int, fees []Fee, durationSec, now int64, salt *big.Int) (Listing, error) {
	lst, _, err := buildAndSignListing(key, chainID, counter, nft, tokenID, priceWei, fees, durationSec, now, salt)
	return lst, err
}

// buildAndSignListing also returns the EIP-712 digest (for tests).
func buildAndSignListing(key *ecdsa.PrivateKey, chainID int, counter *big.Int,
	nft common.Address, tokenID, priceWei *big.Int, fees []Fee, durationSec, now int64, salt *big.Int) (Listing, []byte, error) {

	offerer := gethcrypto.PubkeyToAddress(key.PublicKey)
	startTime := big.NewInt(now)
	endTime := big.NewInt(now + durationSec)

	// consideration: seller proceeds first, then each fee.
	type consItem struct {
		amount    *big.Int
		recipient common.Address
	}
	feeTotal := big.NewInt(0)
	var feeItems []consItem
	for _, f := range fees {
		if f.Bps <= 0 || !common.IsHexAddress(f.Recipient) {
			continue
		}
		amt := new(big.Int).Div(new(big.Int).Mul(priceWei, big.NewInt(f.Bps)), big.NewInt(10000))
		if amt.Sign() == 0 {
			continue
		}
		feeTotal.Add(feeTotal, amt)
		feeItems = append(feeItems, consItem{amt, common.HexToAddress(f.Recipient)})
	}
	toSeller := new(big.Int).Sub(priceWei, feeTotal)
	if toSeller.Sign() < 0 {
		return Listing{}, nil, fmt.Errorf("fees exceed price")
	}

	zero := "0"
	native := common.HexToAddress(zeroAddr).Hex()

	// EIP-712 message arrays (apitypes wants strings for ints, hex for addresses).
	offer := []interface{}{map[string]interface{}{
		"itemType": "2", "token": nft.Hex(), "identifierOrCriteria": tokenID.String(),
		"startAmount": "1", "endAmount": "1",
	}}
	consMsg := []interface{}{map[string]interface{}{
		"itemType": "0", "token": native, "identifierOrCriteria": zero,
		"startAmount": toSeller.String(), "endAmount": toSeller.String(), "recipient": offerer.Hex(),
	}}
	for _, fi := range feeItems {
		consMsg = append(consMsg, map[string]interface{}{
			"itemType": "0", "token": native, "identifierOrCriteria": zero,
			"startAmount": fi.amount.String(), "endAmount": fi.amount.String(), "recipient": fi.recipient.Hex(),
		})
	}

	message := apitypes.TypedDataMessage{
		"offerer": offerer.Hex(), "zone": common.HexToAddress(zeroAddr).Hex(),
		"offer": offer, "consideration": consMsg,
		"orderType": "0", "startTime": startTime.String(), "endTime": endTime.String(),
		"zoneHash": zeroBytes32, "salt": salt.String(), "conduitKey": OSConduitKey, "counter": counter.String(),
	}

	td := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"}, {Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"},
			},
			"OrderComponents": {
				{Name: "offerer", Type: "address"}, {Name: "zone", Type: "address"},
				{Name: "offer", Type: "OfferItem[]"}, {Name: "consideration", Type: "ConsiderationItem[]"},
				{Name: "orderType", Type: "uint8"}, {Name: "startTime", Type: "uint256"},
				{Name: "endTime", Type: "uint256"}, {Name: "zoneHash", Type: "bytes32"},
				{Name: "salt", Type: "uint256"}, {Name: "conduitKey", Type: "bytes32"},
				{Name: "counter", Type: "uint256"},
			},
			"OfferItem": {
				{Name: "itemType", Type: "uint8"}, {Name: "token", Type: "address"},
				{Name: "identifierOrCriteria", Type: "uint256"}, {Name: "startAmount", Type: "uint256"},
				{Name: "endAmount", Type: "uint256"},
			},
			"ConsiderationItem": {
				{Name: "itemType", Type: "uint8"}, {Name: "token", Type: "address"},
				{Name: "identifierOrCriteria", Type: "uint256"}, {Name: "startAmount", Type: "uint256"},
				{Name: "endAmount", Type: "uint256"}, {Name: "recipient", Type: "address"},
			},
		},
		PrimaryType: "OrderComponents",
		Domain: apitypes.TypedDataDomain{
			Name: "Seaport", Version: "1.6",
			ChainId:           gethmath.NewHexOrDecimal256(int64(chainID)),
			VerifyingContract: Seaport16,
		},
		Message: message,
	}

	digest, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		return Listing{}, nil, fmt.Errorf("eip712 hash: %w", err)
	}
	sig, err := gethcrypto.Sign(digest, key)
	if err != nil {
		return Listing{}, nil, err
	}
	sig[64] += 27 // Seaport expects v ∈ {27,28}

	// OpenSea order parameters (string values; includes totalOriginalConsiderationItems).
	params := map[string]interface{}{
		"offerer": offerer.Hex(), "zone": common.HexToAddress(zeroAddr).Hex(),
		"offer": offer, "consideration": consMsg, "orderType": 0,
		"startTime": startTime.String(), "endTime": endTime.String(),
		"zoneHash": zeroBytes32, "salt": salt.String(), "conduitKey": OSConduitKey,
		"totalOriginalConsiderationItems": len(consMsg), "counter": counter.String(),
	}
	return Listing{Parameters: params, Signature: hexutil.Encode(sig), Protocol: Seaport16}, digest, nil
}

// callMsgUnused keeps the ethereum import referenced if other helpers change.
var _ = ethereum.CallMsg{}
