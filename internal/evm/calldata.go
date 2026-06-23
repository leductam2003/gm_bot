package evm

import (
	crand "crypto/rand"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// BuildCalldata constructs tx calldata for a task. Two modes:
//
//	hexMode == true:  rawHex is used verbatim (the "Hex" toggle in the Zyper UI).
//	hexMode == false: functionSig like "mint(uint256,address)" (names optional) is
//	                  ABI-encoded with params. The literal tokens {address}/{wallet}
//	                  in any param are replaced by the wallet address (per zyper-mac
//	                  "{address} = wallet"). uint params accept decimals; bytes accept hex.
//
// params are the values split from the UI's "param1;param2;param3" field, in order.
func BuildCalldata(hexMode bool, rawHex, functionSig string, params []string, wallet common.Address) ([]byte, error) {
	if hexMode {
		data, err := hexutil.Decode(ensure0x(strings.TrimSpace(rawHex)))
		if err != nil {
			return nil, fmt.Errorf("invalid hex calldata: %w", err)
		}
		return data, nil
	}

	if strings.TrimSpace(functionSig) == "" {
		return nil, fmt.Errorf("no function set — enter a function like mint(uint256), enable Hex, or use a SeaDrop mint")
	}
	name, argTypes, err := parseSignature(functionSig)
	if err != nil {
		return nil, err
	}
	if len(params) != len(argTypes) {
		return nil, fmt.Errorf("function %s expects %d params, got %d", name, len(argTypes), len(params))
	}

	// Canonical signature for the 4-byte selector: name(type1,type2,...).
	typeNames := make([]string, len(argTypes))
	for i, t := range argTypes {
		typeNames[i] = t.String()
	}
	canonical := fmt.Sprintf("%s(%s)", name, strings.Join(typeNames, ","))
	selector := crypto.Keccak256([]byte(canonical))[:4]

	args := make(abi.Arguments, len(argTypes))
	values := make([]interface{}, len(argTypes))
	for i, t := range argTypes {
		args[i] = abi.Argument{Type: t}
		raw := substituteWallet(params[i], wallet)
		v, err := convertParam(t, raw)
		if err != nil {
			return nil, fmt.Errorf("param %d (%s): %w", i+1, t.String(), err)
		}
		values[i] = v
	}
	packed, err := args.Pack(values...)
	if err != nil {
		return nil, fmt.Errorf("abi pack: %w", err)
	}
	return append(append([]byte{}, selector...), packed...), nil
}

func substituteWallet(s string, wallet common.Address) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "{address}", wallet.Hex())
	s = strings.ReplaceAll(s, "{wallet}", wallet.Hex())
	s = substituteRandom(s)
	return s
}

// randRe matches {rand:LO-HI} / {random:LO-HI} — a random integer in [LO,HI] (inclusive),
// re-rolled every time calldata is built (each send), e.g. to scan token IDs.
var randRe = regexp.MustCompile(`\{rand(?:om)?:(\d+)-(\d+)\}`)

func substituteRandom(s string) string {
	return randRe.ReplaceAllStringFunc(s, func(tok string) string {
		m := randRe.FindStringSubmatch(tok)
		lo, ok1 := new(big.Int).SetString(m[1], 10)
		hi, ok2 := new(big.Int).SetString(m[2], 10)
		if !ok1 || !ok2 {
			return tok
		}
		if lo.Cmp(hi) > 0 {
			lo, hi = hi, lo
		}
		span := new(big.Int).Add(new(big.Int).Sub(hi, lo), big.NewInt(1)) // inclusive range
		n, err := crand.Int(crand.Reader, span)
		if err != nil {
			return tok
		}
		return new(big.Int).Add(lo, n).String()
	})
}

// parseSignature splits "mint(uint256 qty, address to)" -> ("mint", [uint256, address]).
func parseSignature(sig string) (string, []abi.Type, error) {
	sig = strings.TrimSpace(sig)
	open := strings.Index(sig, "(")
	close := strings.LastIndex(sig, ")")
	if open < 0 || close < open {
		return "", nil, fmt.Errorf("bad function signature %q (want name(type,...))", sig)
	}
	name := strings.TrimSpace(sig[:open])
	if name == "" {
		return "", nil, fmt.Errorf("missing function name in %q", sig)
	}
	inner := strings.TrimSpace(sig[open+1 : close])
	if inner == "" {
		return name, nil, nil
	}
	parts := strings.Split(inner, ",")
	types := make([]abi.Type, 0, len(parts))
	for _, p := range parts {
		// "uint256 qty" or "uint256" -> first token is the type.
		fields := strings.Fields(strings.TrimSpace(p))
		if len(fields) == 0 {
			return "", nil, fmt.Errorf("empty param in signature")
		}
		t, err := abi.NewType(fields[0], "", nil)
		if err != nil {
			return "", nil, fmt.Errorf("unsupported type %q: %w", fields[0], err)
		}
		types = append(types, t)
	}
	return name, types, nil
}

// convertParam turns a string into the exact Go value go-ethereum's abi packer
// expects for the given type.
func convertParam(t abi.Type, raw string) (interface{}, error) {
	switch t.T {
	case abi.AddressTy:
		if !common.IsHexAddress(raw) {
			return nil, fmt.Errorf("invalid address %q", raw)
		}
		return common.HexToAddress(raw), nil
	case abi.BoolTy:
		return strconv.ParseBool(raw)
	case abi.StringTy:
		return raw, nil
	case abi.UintTy, abi.IntTy:
		n, ok := new(big.Int).SetString(raw, 10)
		if !ok {
			return nil, fmt.Errorf("invalid integer %q", raw)
		}
		return fitInteger(t, n)
	case abi.BytesTy:
		b, err := hexutil.Decode(ensure0x(raw))
		if err != nil {
			return nil, fmt.Errorf("invalid bytes %q", raw)
		}
		return b, nil
	case abi.FixedBytesTy:
		b, err := hexutil.Decode(ensure0x(raw))
		if err != nil {
			return nil, fmt.Errorf("invalid bytes%d %q", t.Size, raw)
		}
		if len(b) != t.Size {
			return nil, fmt.Errorf("bytes%d needs %d bytes, got %d", t.Size, t.Size, len(b))
		}
		// Build a [Size]byte via reflection (the type the packer requires).
		arr := reflect.New(t.GetType()).Elem()
		reflect.Copy(arr, reflect.ValueOf(b))
		return arr.Interface(), nil
	default:
		return nil, fmt.Errorf("unsupported param type %q (arrays/tuples not supported yet)", t.String())
	}
}

// fitInteger picks uint8/16/32/64 / *big.Int per the ABI integer size, after
// range-checking so an out-of-range value errors instead of silently truncating
// (e.g. 300 into a uint8 must not become 44).
func fitInteger(t abi.Type, n *big.Int) (interface{}, error) {
	if t.T == abi.UintTy {
		if n.Sign() < 0 {
			return nil, fmt.Errorf("negative value %s for uint%d", n, t.Size)
		}
		if n.BitLen() > t.Size {
			return nil, fmt.Errorf("value %s overflows uint%d", n, t.Size)
		}
		switch t.Size {
		case 8:
			return uint8(n.Uint64()), nil
		case 16:
			return uint16(n.Uint64()), nil
		case 32:
			return uint32(n.Uint64()), nil
		case 64:
			return n.Uint64(), nil
		default:
			return n, nil
		}
	}
	// Signed: a value fits intN if its BitLen is < N (sign bit accounted for).
	if t.Size < 256 && n.BitLen() >= t.Size {
		return nil, fmt.Errorf("value %s overflows int%d", n, t.Size)
	}
	switch t.Size {
	case 8:
		return int8(n.Int64()), nil
	case 16:
		return int16(n.Int64()), nil
	case 32:
		return int32(n.Int64()), nil
	case 64:
		return n.Int64(), nil
	default:
		return n, nil
	}
}

func ensure0x(s string) string {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return s
	}
	return "0x" + s
}
