package evm

import (
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

var errManualMissing = errors.New("manual gas mode requires maxFeeGwei and priorityFeeGwei")

// IsAlreadyKnown reports whether a broadcast error means the tx is already in the
// mempool (treat as sent). Port of seaport.ts isAlreadyKnown.
func IsAlreadyKnown(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "already known") ||
		strings.Contains(m, "known transaction") ||
		strings.Contains(m, "already imported") ||
		strings.Contains(m, "alreadyknown")
}

// IsNonceError reports whether the error calls for a nonce refetch + re-sign.
func IsNonceError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "nonce too low") ||
		strings.Contains(m, "replacement") ||
		strings.Contains(m, "already imported") ||
		strings.Contains(m, "known transaction")
}

// Standard revert ABIs: Error(string) = 0x08c379a0, Panic(uint256) = 0x4e487b71.
var (
	errStringArgs, _ = abi.NewType("string", "", nil)
	panicArgs, _     = abi.NewType("uint256", "", nil)
)

// DecodeRevert turns revert return data into a human reason, or "" if it can't.
func DecodeRevert(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	sel := data[:4]
	switch {
	case sel[0] == 0x08 && sel[1] == 0xc3 && sel[2] == 0x79 && sel[3] == 0xa0: // Error(string)
		vals, err := (abi.Arguments{{Type: errStringArgs}}).Unpack(data[4:])
		if err == nil && len(vals) == 1 {
			if s, ok := vals[0].(string); ok {
				return s
			}
		}
	case sel[0] == 0x4e && sel[1] == 0x48 && sel[2] == 0x7b && sel[3] == 0x71: // Panic(uint256)
		vals, err := (abi.Arguments{{Type: panicArgs}}).Unpack(data[4:])
		if err == nil && len(vals) == 1 {
			return "Panic(" + toString(vals[0]) + ")"
		}
	}
	return ""
}

func toString(v any) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	return ""
}
