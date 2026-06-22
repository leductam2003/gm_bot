// Package evm holds the low-level Ethereum building blocks ported from zyper-mac:
// gas fee resolution (gas.ts), calldata construction (contractAbi.ts), per-wallet
// nonce reservation (nonce.ts), and the sign+broadcast+retry loop (mintEngine.ts).
package evm

import "math/big"

// GasMode mirrors zyper-mac GasParams.mode.
type GasMode string

const (
	GasAuto   GasMode = "auto"
	GasManual GasMode = "manual"
)

// Defaults from gasutil.init.0 (gas.ts).
const (
	DefaultMultiplier      = 2.0
	DefaultMinPriorityGwei = 0.1
	DefaultEstimatePad     = 1.3
)

// GasParams is the port of zyper-mac/src/types.ts GasParams.
type GasParams struct {
	Mode GasMode `json:"mode"`
	// Manual mode (gwei).
	MaxFeeGwei      *float64 `json:"maxFeeGwei,omitempty"`
	PriorityFeeGwei *float64 `json:"priorityFeeGwei,omitempty"`
	// Auto mode tuning.
	AutoMultiplier  *float64 `json:"autoMultiplier,omitempty"`  // default 2.0
	MinPriorityGwei *float64 `json:"minPriorityGwei,omitempty"` // default 0.1
	PinPriority     bool     `json:"pinPriority,omitempty"`     // pin tip to exactly the floor
	EstimatePad     *float64 `json:"estimatePad,omitempty"`     // default 1.3
	// Explicit overrides.
	GasLimit *uint64 `json:"gasLimit,omitempty"` // skip estimateGas when set
}

// ResolvedFees is the EIP-1559 fee pair in wei.
type ResolvedFees struct {
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
}

func f(v float64) *float64 { return &v }

// LowGasProfile = the "Đã kiểm tra" careful/cheap non-snipe profile (gas.ts).
func LowGasProfile() GasParams {
	return GasParams{
		Mode:            GasAuto,
		AutoMultiplier:  f(1.2),
		MinPriorityGwei: f(0.001),
		EstimatePad:     f(1.2),
		PinPriority:     true,
	}
}
