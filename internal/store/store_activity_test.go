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
	if cost, found := st.MatchMintCost(1, "0xabc", "6", "0xWALLET"); cost != "1000" || !found {
		t.Fatalf("matched cost = %s found = %v, want 1000/true", cost, found)
	}
	// Selling the same token again finds no unsold match -> not found.
	if cost, found := st.MatchMintCost(1, "0xabc", "6", "0xwallet"); found || cost != "0" {
		t.Fatalf("re-sold cost = %s found = %v, want 0/false", cost, found)
	}
	// SaleExists dedup: unknown sale absent, then present after AddSale.
	if ex, _ := st.SaleExists("0xDEAD", "0xabc", "6"); ex {
		t.Fatal("SaleExists true before any sale")
	}

	if _, err := st.AddSale(Sale{Ts: now, ChainID: 1, Contract: "0xabc", Collection: "Plimpo", TokenID: "6", TxHash: "0xSALEtx", ProceedsWei: "5000", CostWei: "1000"}); err != nil {
		t.Fatal(err)
	}
	sales, _ := st.AllSales()
	if len(sales) != 1 || sales[0].ProceedsWei != "5000" || sales[0].CostWei != "1000" || sales[0].Collection != "Plimpo" {
		t.Fatalf("sales = %+v", sales)
	}
	// Re-inserting the same (tx, contract, token) must be a no-op (unique index dedup), so the
	// two recorders can't double-count a sale.
	if _, err := st.AddSale(Sale{Ts: now, ChainID: 1, Contract: "0xABC", Collection: "Plimpo", TokenID: "6", TxHash: "0xsaletx", ProceedsWei: "9999", CostWei: "1000"}); err != nil {
		t.Fatal(err)
	}
	if s2, _ := st.AllSales(); len(s2) != 1 {
		t.Fatalf("duplicate sale not deduped: %d rows", len(s2))
	}
	// Dedup: the just-booked sale is now found (case-insensitive tx match).
	if ex, _ := st.SaleExists("0xsaletx", "0xABC", "6"); !ex {
		t.Fatal("SaleExists false after AddSale")
	}
	// MintedContracts surfaces the (chain, contract) we minted into.
	if mc, _ := st.MintedContracts(); len(mc) != 1 || mc[0].ChainID != 1 || mc[0].Contract != "0xabc" {
		t.Fatalf("MintedContracts = %+v", mc)
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
