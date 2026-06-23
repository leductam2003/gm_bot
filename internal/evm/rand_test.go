package evm

import (
	"math/big"
	"strings"
	"testing"
)

func TestSubstituteRandom(t *testing.T) {
	// fixed single value
	if got := substituteRandom("{rand:5-5}"); got != "5" {
		t.Fatalf("{rand:5-5} = %q, want 5", got)
	}
	// {random:...} alias + leading zeros parse as the number
	if got := substituteRandom("{random:0007-0007}"); got != "7" {
		t.Fatalf("{random:0007-0007} = %q, want 7", got)
	}
	// non-placeholder text is left alone
	if got := substituteRandom("123"); got != "123" {
		t.Fatalf("plain = %q, want 123", got)
	}
	// range is inclusive [lo,hi] and re-rolled; check 1000 samples stay in bounds and vary
	lo, hi := big.NewInt(0), big.NewInt(10000)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		got := substituteRandom("{rand:0-10000}")
		v, ok := new(big.Int).SetString(got, 10)
		if !ok {
			t.Fatalf("not a number: %q", got)
		}
		if v.Cmp(lo) < 0 || v.Cmp(hi) > 0 {
			t.Fatalf("%s out of [0,10000]", got)
		}
		seen[got] = true
	}
	if len(seen) < 100 { // should be highly varied
		t.Fatalf("expected varied randoms, got only %d distinct", len(seen))
	}
	// works embedded in a larger string
	out := substituteRandom("mintTo(7,{rand:3-3})")
	if !strings.Contains(out, "3") || strings.Contains(out, "{") {
		t.Fatalf("embedded = %q", out)
	}
}
