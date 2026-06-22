package engine

import (
	"math/big"
	"testing"

	"zyperbot/internal/evm"
)

func TestBumpFeesAboveReplacementFloor(t *testing.T) {
	// geth requires a >= 10% bump to replace; we target +12.5% minimum.
	in := evm.ResolvedFees{
		MaxFeePerGas:         big.NewInt(100_000_000_000), // 100 gwei
		MaxPriorityFeePerGas: big.NewInt(2_000_000_000),   // 2 gwei
	}
	baseFee := big.NewInt(50_000_000_000) // 50 gwei
	out := bumpFees(in, baseFee)
	// priority must rise at least 12.5%.
	minPrio := new(big.Int).Div(new(big.Int).Mul(in.MaxPriorityFeePerGas, big.NewInt(1125)), big.NewInt(1000))
	if out.MaxPriorityFeePerGas.Cmp(minPrio) < 0 {
		t.Fatalf("priority bump too small: %s < %s", out.MaxPriorityFeePerGas, minPrio)
	}
	// maxFee must also rise at least 12.5%.
	minMax := new(big.Int).Div(new(big.Int).Mul(in.MaxFeePerGas, big.NewInt(1125)), big.NewInt(1000))
	if out.MaxFeePerGas.Cmp(minMax) < 0 {
		t.Fatalf("maxFee bump too small: %s < %s", out.MaxFeePerGas, minMax)
	}
}

func TestBumpFeesPriorityMatchesOriginal(t *testing.T) {
	// Port check vs mintEngine.ts: at low priority, newPrio = max(prio*2, prio+0.3gwei).
	// prio=1gwei → dbl=2gwei, plus=1.3gwei → newPrio=2gwei (NOT 1.125gwei).
	in := evm.ResolvedFees{
		MaxFeePerGas:         big.NewInt(10_000_000_000), // 10 gwei
		MaxPriorityFeePerGas: big.NewInt(1_000_000_000),  // 1 gwei
	}
	out := bumpFees(in, big.NewInt(20_000_000_000)) // base 20 gwei
	if out.MaxPriorityFeePerGas.Cmp(big.NewInt(2_000_000_000)) != 0 {
		t.Fatalf("newPrio=%s want 2 gwei (prio*2)", out.MaxPriorityFeePerGas)
	}
	// newMax = base*2 + newPrio = 40 + 2 = 42 gwei (floored at maxFee*1.125=11.25).
	if out.MaxFeePerGas.Cmp(big.NewInt(42_000_000_000)) != 0 {
		t.Fatalf("newMax=%s want 42 gwei (base*2+newPrio)", out.MaxFeePerGas)
	}
}

func TestParseBigOr0(t *testing.T) {
	cases := map[string]string{"": "0", "  ": "0", "123": "123", "abc": "0", "1000000000000000000": "1000000000000000000"}
	for in, want := range cases {
		if got := parseBigOr0(in).String(); got != want {
			t.Errorf("parseBigOr0(%q)=%s want %s", in, got, want)
		}
	}
}

func TestWeiToEth(t *testing.T) {
	if got := weiToEth(big.NewInt(1_500_000_000_000_000_000)); got != "1.500000" {
		t.Fatalf("weiToEth=%s want 1.500000", got)
	}
}

func TestShortURL(t *testing.T) {
	if got := shortURL("https://eth-mainnet.g.alchemy.com/v2/KEY123"); got != "eth-mainnet.g.alchemy.com" {
		t.Fatalf("shortURL=%s", got)
	}
}
