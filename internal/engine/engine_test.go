package engine

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

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

func TestOpenSeaSlugFrom(t *testing.T) {
	// Registry chain → its slug (works without any custom-chain config).
	if got := openSeaSlugFrom(1, nil); got != "ethereum" {
		t.Fatalf("chain 1 slug = %q, want ethereum", got)
	}
	if got := openSeaSlugFrom(8453, nil); got != "base" {
		t.Fatalf("chain 8453 slug = %q, want base", got)
	}
	// Custom chain (Robinhood 4663) is NOT in the registry — must resolve from its
	// configured name, else the voucher queries "ethereum" and OpenSea says DropNotFound.
	cfg := map[string]any{"customChains": []any{
		map[string]any{"id": float64(4663), "name": "Robinhood"},
	}}
	if got := openSeaSlugFrom(4663, cfg); got != "robinhood" {
		t.Fatalf("robinhood slug = %q, want robinhood (the DropNotFound bug)", got)
	}
	// Unknown custom chain with no config → "" (caller must NOT guess a chain).
	if got := openSeaSlugFrom(4663, nil); got != "" {
		t.Fatalf("unconfigured custom chain slug = %q, want empty", got)
	}
	// Name with spaces normalizes to a slug.
	cfg2 := map[string]any{"customChains": []any{map[string]any{"id": float64(9999), "name": "My Chain"}}}
	if got := openSeaSlugFrom(9999, cfg2); got != "mychain" {
		t.Fatalf("spaced name slug = %q, want mychain", got)
	}
}

func TestRedactErr(t *testing.T) {
	// go-ethereum transport errors embed the full RPC URL whose path can be a secret
	// token — the redaction must strip it to host-only before it hits the UI/log.
	in := `Post "https://hood.allnodes.me:8547/xZli7t7St0qt23lp": dial tcp: timeout`
	out := redactErr(in)
	if strings.Contains(out, "xZli7t7St0qt23lp") {
		t.Fatalf("secret token survived redaction: %s", out)
	}
	if !strings.Contains(out, "hood.allnodes.me:8547") {
		t.Fatalf("host should remain for diagnosis: %s", out)
	}
	// http and multiple URLs
	if got := redactErr(`x http://a.b/SECRET y https://c.d/KEY z`); strings.Contains(got, "SECRET") || strings.Contains(got, "KEY") {
		t.Fatalf("token survived: %s", got)
	}
	// no URL → unchanged
	if got := redactErr("nonce too low"); got != "nonce too low" {
		t.Fatalf("plain message altered: %s", got)
	}
}

func TestShortURL(t *testing.T) {
	if got := shortURL("https://eth-mainnet.g.alchemy.com/v2/KEY123"); got != "eth-mainnet.g.alchemy.com" {
		t.Fatalf("shortURL=%s", got)
	}
}

func TestWaitUntilLandsOnBoundary(t *testing.T) {
	// The snipe wake-up must land ON the target second, not up to ~1s past it.
	sec := time.Now().Unix() + 2
	if !waitUntil(context.Background(), sec) {
		t.Fatal("waitUntil returned false without cancellation")
	}
	late := time.Since(time.Unix(sec, 0))
	if late < 0 {
		t.Fatalf("woke %s BEFORE the target second", -late)
	}
	if late > 500*time.Millisecond {
		t.Fatalf("woke %s after the target second (want < 500ms)", late)
	}
}

func TestWaitUntilCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if waitUntil(ctx, time.Now().Unix()+60) {
		t.Fatal("expected false on cancel")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel took too long to unblock the wait")
	}
}

func TestAffordableGasLimit(t *testing.T) {
	gwei := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e9)) }
	eth := func(f float64) *big.Int { w, _ := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18)).Int(nil); return w }
	maxFee := gwei(50)
	const want = 500_000

	// Well-funded: cap does not lower the 500k fallback.
	if g := affordableGasLimit(eth(1), eth(0.05), maxFee, want); g != want {
		t.Fatalf("well-funded: got %d want %d", g, want)
	}
	// Tightly funded: value 0.05 + room for exactly 150k gas. Must cap to 150k so the
	// SIGNED tx (value + 150k*maxFee) is still <= balance — the T0-rejection bug.
	bal := new(big.Int).Add(eth(0.05), new(big.Int).Mul(big.NewInt(150_000), maxFee))
	if g := affordableGasLimit(bal, eth(0.05), maxFee, want); g != 150_000 {
		t.Fatalf("tight: got %d want 150000", g)
	}
	// Verify the capped tx is actually admissible: value + g*maxFee <= bal.
	if g := affordableGasLimit(bal, eth(0.05), maxFee, want); new(big.Int).Add(eth(0.05), new(big.Int).Mul(big.NewInt(int64(g)), maxFee)).Cmp(bal) > 0 {
		t.Fatal("capped tx exceeds balance — would be rejected at broadcast")
	}
	// Can't cover the value at all → 0 (floor benches it).
	if g := affordableGasLimit(eth(0.01), eth(0.05), maxFee, want); g != 0 {
		t.Fatalf("underfunded: got %d want 0", g)
	}
	// Zero maxFee (manual gas edge) → passthrough, no divide-by-zero.
	if g := affordableGasLimit(eth(1), eth(0), big.NewInt(0), want); g != want {
		t.Fatalf("zero maxFee: got %d want %d", g, want)
	}
}

func TestArmLeadSpread(t *testing.T) {
	// Leads must stay within [armLeadSec, armLeadSec+armSpreadSec) and actually
	// spread — 1000 tasks waking at the same instant is the failure being prevented.
	seen := map[int64]bool{}
	for id := int64(0); id < 1000; id++ {
		l := armLeadFor(id)
		if l < armLeadSec || l >= armLeadSec+armSpreadSec {
			t.Fatalf("armLeadFor(%d)=%d out of range", id, l)
		}
		seen[l] = true
	}
	if len(seen) < armSpreadSec {
		t.Fatalf("leads not spread: only %d distinct values", len(seen))
	}
	if armLeadFor(-7) != armLeadFor(7) {
		t.Fatal("negative id must not underflow the lead")
	}
}

func TestModeIsInstant(t *testing.T) {
	// "action" is the legacy alias for Instant — tasks persisted before the rename
	// must keep sending straight away, and only these two modes may skip preflight.
	for _, m := range []Mode{ModeInstant, ModeAction} {
		if !m.isInstant() {
			t.Fatalf("%s should be instant", m)
		}
	}
	for _, m := range []Mode{ModeSimulate, ModeSpam, Mode("")} {
		if m.isInstant() {
			t.Fatalf("%s should NOT be instant", m)
		}
	}
}

func TestCanPreArm(t *testing.T) {
	if !canPreArm(TaskConfig{Mode: ModeAction}) {
		t.Fatal("plain action task should pre-arm")
	}
	if !canPreArm(TaskConfig{Seadrop: true}) {
		t.Fatal("seadrop should pre-arm (on-chain mintPublic path; voucher stays T0-only)")
	}
	if canPreArm(TaskConfig{Flashbots: true}) {
		t.Fatal("flashbots must not pre-arm (targets head block at send)")
	}
	if canPreArm(TaskConfig{WalletOverrides: map[int64]TaskConfig{7: {Flashbots: true}}}) {
		t.Fatal("flashbots wallet override must not pre-arm")
	}
	// A stale flashbots override for a wallet NOT in this task must not disable pre-arm.
	if !canPreArm(TaskConfig{WalletIDs: []int64{1}, WalletOverrides: map[int64]TaskConfig{7: {Flashbots: true}}}) {
		t.Fatal("stale override for a removed wallet must not disable pre-arm")
	}
}
