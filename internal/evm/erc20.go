// Package evm — minimal ERC-20 read/encode helpers for the Manage Funds
// (disperse / consolidate) feature: decimals, balanceOf, transfer calldata, and a
// gas estimate wrapper. Native (ETH) transfers don't need any of this.
package evm

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const erc20MiniABI = `[
 {"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]},
 {"name":"balanceOf","type":"function","stateMutability":"view","inputs":[{"name":"o","type":"address"}],"outputs":[{"type":"uint256"}]},
 {"name":"transfer","type":"function","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"type":"bool"}]}
]`

var erc20Parsed abi.ABI

func init() { erc20Parsed, _ = abi.JSON(strings.NewReader(erc20MiniABI)) }

// ERC20Decimals reads token.decimals(). Defaults to 18 if the token doesn't expose it.
func ERC20Decimals(ctx context.Context, c *ethclient.Client, token common.Address) (int, error) {
	out, err := callERC20(ctx, c, token, "decimals")
	if err != nil {
		return 0, err
	}
	if len(out) == 1 {
		if v, ok := out[0].(uint8); ok {
			return int(v), nil
		}
	}
	return 18, nil
}

// ERC20BalanceOf returns the token balance (raw units) of owner.
func ERC20BalanceOf(ctx context.Context, c *ethclient.Client, token, owner common.Address) (*big.Int, error) {
	out, err := callERC20(ctx, c, token, "balanceOf", owner)
	if err != nil {
		return nil, err
	}
	if len(out) == 1 {
		if v, ok := out[0].(*big.Int); ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("unexpected balanceOf result")
}

// ERC20TransferData encodes transfer(to, amount) calldata (raw token units).
func ERC20TransferData(to common.Address, amount *big.Int) ([]byte, error) {
	return erc20Parsed.Pack("transfer", to, amount)
}

func callERC20(ctx context.Context, c *ethclient.Client, token common.Address, method string, args ...interface{}) ([]interface{}, error) {
	data, err := erc20Parsed.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	ret, err := c.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return erc20Parsed.Unpack(method, ret)
}

// EstimateGas wraps eth_estimateGas for a single call (used to size ERC-20 transfers).
func EstimateGas(ctx context.Context, c *ethclient.Client, from, to common.Address, data []byte, value *big.Int) (uint64, error) {
	return c.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Data: data, Value: value})
}
