package evm

import (
	"context"
	"fmt"
	"math/big"
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Canonical OpenSea SeaDrop 1.0 — same address on every supported EVM chain.
const SeaDropAddress = "0x00005EA00Ac477B1030CE78506496e8C2dE24bf5"

// OpenSea's default fee recipient (used when the drop doesn't restrict recipients).
const OSFeeRecipient = "0x0000a26b00c1F0DF003000390027140000fAa719"

const zeroAddr = "0x0000000000000000000000000000000000000000"

var (
	erc721ABI        abi.ABI
	seaDropABI       abi.ABI
	erc721SeaDropABI abi.ABI
)

func init() {
	erc721ABI = mustABI(`[
		{"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint256"}]},
		{"type":"function","name":"name","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]}
	]`)
	seaDropABI = mustABI(`[
		{"type":"function","name":"mintPublic","stateMutability":"payable","inputs":[
			{"name":"nftContract","type":"address"},{"name":"feeRecipient","type":"address"},
			{"name":"minterIfNotPayer","type":"address"},{"name":"quantity","type":"uint256"}],"outputs":[]},
		{"type":"function","name":"getPublicDrop","stateMutability":"view","inputs":[{"name":"nftContract","type":"address"}],"outputs":[
			{"name":"","type":"tuple","components":[
				{"name":"mintPrice","type":"uint80"},{"name":"startTime","type":"uint48"},{"name":"endTime","type":"uint48"},
				{"name":"maxTotalMintableByWallet","type":"uint16"},{"name":"feeBps","type":"uint16"},{"name":"restrictFeeRecipients","type":"bool"}]}]},
		{"type":"function","name":"getAllowedFeeRecipients","stateMutability":"view","inputs":[{"name":"nftContract","type":"address"}],"outputs":[{"type":"address[]"}]}
	]`)
	erc721SeaDropABI = mustABI(`[
		{"type":"function","name":"getAllowedSeaDrop","stateMutability":"view","inputs":[],"outputs":[{"type":"address[]"}]}
	]`)
}

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic("nft abi: " + err.Error())
	}
	return a
}

func callView(ctx context.Context, c *ethclient.Client, to common.Address, a abi.ABI, method string, args ...interface{}) ([]interface{}, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	ret, err := c.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return a.Unpack(method, ret)
}

// BalanceOf returns the ERC721/1155 balance of owner in the collection.
func BalanceOf(ctx context.Context, c *ethclient.Client, contract, owner common.Address) (*big.Int, error) {
	vals, err := callView(ctx, c, contract, erc721ABI, "balanceOf", owner)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, fmt.Errorf("empty balanceOf result")
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("unexpected balanceOf type")
	}
	return n, nil
}

// CollectionName returns the collection's name() (best-effort).
func CollectionName(ctx context.Context, c *ethclient.Client, contract common.Address) string {
	vals, err := callView(ctx, c, contract, erc721ABI, "name")
	if err != nil || len(vals) == 0 {
		return ""
	}
	if s, ok := vals[0].(string); ok {
		return s
	}
	return ""
}

// PublicDrop is the live SeaDrop public-drop config.
type PublicDrop struct {
	MintPrice                *big.Int `json:"mintPriceWei"`
	StartTime                uint64   `json:"startTime"`
	EndTime                  uint64   `json:"endTime"`
	MaxTotalMintableByWallet uint16   `json:"maxPerWallet"`
	FeeBps                   uint16   `json:"feeBps"`
	RestrictFeeRecipients    bool     `json:"restrictFeeRecipients"`
}

// toBig coerces any ABI-decoded integer (*big.Int or a fixed uintN/intN) to *big.Int,
// so we don't depend on go-ethereum's exact reflect type for uint48/uint80.
func toBig(v reflect.Value) *big.Int {
	if !v.IsValid() {
		return big.NewInt(0)
	}
	switch v.Kind() {
	case reflect.Ptr:
		if b, ok := v.Interface().(*big.Int); ok && b != nil {
			return b
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return new(big.Int).SetUint64(v.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return big.NewInt(v.Int())
	}
	return big.NewInt(0)
}

// ReadPublicDrop reads the live public drop + allowed fee recipients for a collection.
func ReadPublicDrop(ctx context.Context, c *ethclient.Client, nft common.Address) (PublicDrop, []common.Address, error) {
	sd := common.HexToAddress(SeaDropAddress)
	data, err := seaDropABI.Pack("getPublicDrop", nft)
	if err != nil {
		return PublicDrop{}, nil, err
	}
	ret, err := c.CallContract(ctx, ethereum.CallMsg{To: &sd, Data: data}, nil)
	if err != nil {
		return PublicDrop{}, nil, err
	}
	vals, err := seaDropABI.Unpack("getPublicDrop", ret)
	if err != nil {
		return PublicDrop{}, nil, err
	}
	if len(vals) == 0 {
		return PublicDrop{}, nil, fmt.Errorf("empty getPublicDrop result")
	}
	rv := reflect.ValueOf(vals[0])
	if rv.Kind() != reflect.Struct {
		return PublicDrop{}, nil, fmt.Errorf("unexpected getPublicDrop shape")
	}
	drop := PublicDrop{
		MintPrice:                toBig(rv.FieldByName("MintPrice")),
		StartTime:                toBig(rv.FieldByName("StartTime")).Uint64(),
		EndTime:                  toBig(rv.FieldByName("EndTime")).Uint64(),
		MaxTotalMintableByWallet: uint16(toBig(rv.FieldByName("MaxTotalMintableByWallet")).Uint64()),
		FeeBps:                   uint16(toBig(rv.FieldByName("FeeBps")).Uint64()),
		RestrictFeeRecipients:    rv.FieldByName("RestrictFeeRecipients").IsValid() && rv.FieldByName("RestrictFeeRecipients").Bool(),
	}
	var recips []common.Address
	if vals, err := callView(ctx, c, sd, seaDropABI, "getAllowedFeeRecipients", nft); err == nil && len(vals) > 0 {
		if arr, ok := vals[0].([]common.Address); ok {
			recips = arr
		}
	}
	return drop, recips, nil
}

// IsSeaDropMintable reports whether the collection routes minting through SeaDrop —
// either it advertises getAllowedSeaDrop(SeaDropAddress) or it has a live public drop.
func IsSeaDropMintable(ctx context.Context, c *ethclient.Client, nft common.Address) bool {
	if vals, err := callView(ctx, c, nft, erc721SeaDropABI, "getAllowedSeaDrop"); err == nil && len(vals) > 0 {
		if arr, ok := vals[0].([]common.Address); ok {
			for _, a := range arr {
				if strings.EqualFold(a.Hex(), SeaDropAddress) {
					return true
				}
			}
		}
	}
	drop, _, err := ReadPublicDrop(ctx, c, nft)
	if err != nil {
		return false
	}
	return drop.StartTime > 0 || drop.EndTime > 0 || drop.MaxTotalMintableByWallet > 0
}

// SeaDropResolved is a ready-to-send mintPublic transaction template.
type SeaDropResolved struct {
	To           common.Address
	Data         []byte
	Value        *big.Int
	Drop         PublicDrop
	FeeRecipient common.Address
}

// ResolveSeaDrop reads the public drop and builds mintPublic(nft, feeRecipient, 0x0, qty)
// with value = price*qty. feeOverride (non-zero) overrides the fee recipient.
func ResolveSeaDrop(ctx context.Context, c *ethclient.Client, nft common.Address, qty int, feeOverride common.Address) (SeaDropResolved, error) {
	if qty < 1 {
		qty = 1
	}
	drop, recips, err := ReadPublicDrop(ctx, c, nft)
	if err != nil {
		return SeaDropResolved{}, err
	}
	fee := feeOverride
	if (fee == common.Address{}) {
		if len(recips) > 0 {
			fee = recips[0]
		} else {
			fee = common.HexToAddress(OSFeeRecipient)
		}
	}
	data, err := seaDropABI.Pack("mintPublic", nft, fee, common.HexToAddress(zeroAddr), big.NewInt(int64(qty)))
	if err != nil {
		return SeaDropResolved{}, err
	}
	price := drop.MintPrice
	if price == nil {
		price = big.NewInt(0)
	}
	value := new(big.Int).Mul(price, big.NewInt(int64(qty)))
	return SeaDropResolved{
		To:           common.HexToAddress(SeaDropAddress),
		Data:         data,
		Value:        value,
		Drop:         drop,
		FeeRecipient: fee,
	}, nil
}
