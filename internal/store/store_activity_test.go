package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestActivityAndRealizedPnl(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	// Two successful mints (mixed-case contract on purpose) + one failure.
	if _, err := st.AddMint(Mint{Ts: now, ChainID: 1, Contract: "0xAbC", TokenID: "6", WalletID: 1, Address: "0xWallet", TxHash: "0xtx1", CostWei: "1000", Status: "minted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMint(Mint{Ts: now, ChainID: 1, Contract: "0xABC", TokenID: "7", WalletID: 2, Address: "0xW2", CostWei: "2000", Status: "minted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMint(Mint{Ts: now, ChainID: 1, Status: "failed"}); err != nil {
		t.Fatal(err)
	}

	if n, _ := st.CountMints("minted"); n != 2 {
		t.Fatalf("minted count = %d, want 2", n)
	}
	if n, _ := st.CountMints("failed"); n != 1 {
		t.Fatalf("failed count = %d, want 1", n)
	}
	if rec, _ := st.RecentMints(10); len(rec) != 2 { // failures excluded from activity
		t.Fatalf("recent minted = %d, want 2", len(rec))
	}

	// Sell token 6 — case-insensitive contract + address match -> cost basis 1000, marks sold.
	if cost := st.MatchMintCost(1, "0xabc", "6", "0xWALLET"); cost != "1000" {
		t.Fatalf("matched cost = %s, want 1000", cost)
	}
	// Selling the same token again finds no unsold match -> "0".
	if cost := st.MatchMintCost(1, "0xabc", "6", "0xwallet"); cost != "0" {
		t.Fatalf("re-sold cost = %s, want 0", cost)
	}

	if _, err := st.AddSale(Sale{Ts: now, ChainID: 1, Contract: "0xabc", Collection: "Plimpo", TokenID: "6", ProceedsWei: "5000", CostWei: "1000"}); err != nil {
		t.Fatal(err)
	}
	sales, _ := st.AllSales()
	if len(sales) != 1 || sales[0].ProceedsWei != "5000" || sales[0].CostWei != "1000" || sales[0].Collection != "Plimpo" {
		t.Fatalf("sales = %+v", sales)
	}

	if err := st.ResetActivity(); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountMints("minted"); n != 0 {
		t.Fatalf("after reset minted = %d, want 0", n)
	}
	if s, _ := st.AllSales(); len(s) != 0 {
		t.Fatalf("after reset sales = %d, want 0", len(s))
	}
}
