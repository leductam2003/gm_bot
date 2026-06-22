package evm

import (
	"context"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/ethclient"
)

// gweiToWei converts a (possibly fractional) gwei amount to wei, matching viem's
// parseGwei. 0.1 gwei -> 1e8 wei, 2.0 -> 2e9. Uses 9-decimal fixed-point rounding.
func gweiToWei(gwei float64) *big.Int {
	// Round to the nearest wei: round(gwei * 1e9).
	wei := math.Round(gwei * 1e9)
	bi, _ := new(big.Float).SetFloat64(wei).Int(nil)
	return bi
}

// mulRat multiplies a wei value by a rational multiplier using integer math
// (×round(mult*1000)/1000) — the exact port of gas.ts mulRat.
func mulRat(value *big.Int, mult float64) *big.Int {
	scale := big.NewInt(int64(math.Round(mult * 1000)))
	out := new(big.Int).Mul(value, scale)
	return out.Div(out, big.NewInt(1000))
}

// ResolveFees is the faithful port of gas.ts resolveFees:
//
//	manual: maxFee/priority straight from the supplied gwei values.
//	auto:   tip = max(SuggestGasTipCap*mult, floor) [or floor if pinPriority];
//	        maxFee = baseFee*mult + tip.
func ResolveFees(ctx context.Context, c *ethclient.Client, gas GasParams) (ResolvedFees, error) {
	if gas.Mode == GasManual {
		if gas.MaxFeeGwei == nil || gas.PriorityFeeGwei == nil {
			return ResolvedFees{}, errManualMissing
		}
		return ResolvedFees{
			MaxFeePerGas:         gweiToWei(*gas.MaxFeeGwei),
			MaxPriorityFeePerGas: gweiToWei(*gas.PriorityFeeGwei),
		}, nil
	}

	mult := DefaultMultiplier
	if gas.AutoMultiplier != nil {
		mult = *gas.AutoMultiplier
	}
	minPriority := DefaultMinPriorityGwei
	if gas.MinPriorityGwei != nil {
		minPriority = *gas.MinPriorityGwei
	}
	floor := gweiToWei(minPriority)

	// baseFee from the latest block header.
	head, err := c.HeaderByNumber(ctx, nil)
	if err != nil {
		return ResolvedFees{}, err
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}

	var tip *big.Int
	if gas.PinPriority {
		tip = floor
	} else {
		tipCap, err := c.SuggestGasTipCap(ctx)
		if err != nil || tipCap == nil {
			tipCap = gweiToWei(1.5) // fallback: parseGwei("1.5")
		}
		tipScaled := mulRat(tipCap, mult)
		if tipScaled.Cmp(floor) > 0 {
			tip = tipScaled
		} else {
			tip = floor
		}
	}

	maxFee := new(big.Int).Add(mulRat(baseFee, mult), tip)
	return ResolvedFees{MaxFeePerGas: maxFee, MaxPriorityFeePerGas: tip}, nil
}

// EstimateGasLimit estimates and pads (default +30%, floor 21000) — port of the
// non-snipe gasLimit path in mintEngine.ts.
func EstimateGasLimit(ctx context.Context, c *ethclient.Client, msg ethereum.CallMsg, pad float64) (uint64, error) {
	est, err := c.EstimateGas(ctx, msg)
	if err != nil {
		return 0, err
	}
	if pad <= 0 {
		pad = DefaultEstimatePad
	}
	padded := uint64(math.Round(float64(est) * pad))
	if padded < 21000 {
		padded = 21000
	}
	return padded, nil
}
