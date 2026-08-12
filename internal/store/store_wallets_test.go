package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestAddWalletsBatch verifies the batched insert used by bulk generate/import:
// it inserts every new wallet in one transaction and reports an accurate "added"
// count, while silently skipping duplicates (same network+address).
func TestAddWalletsBatch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mk := func(n int) []Wallet {
		ws := make([]Wallet, 0, n)
		for i := 0; i < n; i++ {
			ws = append(ws, Wallet{
				Label:      "wallet",
				Network:    "evm",
				Address:    fmt.Sprintf("0x%040x", i),
				EncPrivKey: "enc",
				GroupName:  "batch",
			})
		}
		return ws
	}

	// First batch: all 300 are new.
	added, err := st.AddWallets(mk(300))
	if err != nil {
		t.Fatal(err)
	}
	if added != 300 {
		t.Fatalf("first batch added = %d, want 300", added)
	}

	// Overlapping batch: 300 dupes + 200 new -> only 200 counted as added.
	added, err = st.AddWallets(mk(500))
	if err != nil {
		t.Fatal(err)
	}
	if added != 200 {
		t.Fatalf("overlapping batch added = %d, want 200", added)
	}

	all, err := st.ListWallets()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 500 {
		t.Fatalf("total wallets = %d, want 500", len(all))
	}

	// Empty batch is a no-op, not an error.
	if added, err := st.AddWallets(nil); err != nil || added != 0 {
		t.Fatalf("empty batch = (%d,%v), want (0,nil)", added, err)
	}
}

// TestRenameWallets verifies the bulk label update: only the requested ids change,
// missing ids are skipped (not errors), and the count reflects real updates.
func TestRenameWallets(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := st.AddWallet(Wallet{
			Label: "wallet", Network: "evm",
			Address: fmt.Sprintf("0x%040x", 1000+i), EncPrivKey: "enc", GroupName: "g",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// Rename two of three, plus one id that doesn't exist.
	n, err := st.RenameWallets([]int64{ids[0], ids[1], 99999}, "sniper")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("renamed = %d, want 2", n)
	}
	all, err := st.ListWallets()
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, w := range all {
		got[w.ID] = w.Label
	}
	if got[ids[0]] != "sniper" || got[ids[1]] != "sniper" {
		t.Fatalf("renamed labels = %q,%q, want sniper,sniper", got[ids[0]], got[ids[1]])
	}
	if got[ids[2]] != "wallet" {
		t.Fatalf("untouched wallet label = %q, want wallet", got[ids[2]])
	}

	// Empty id list is a no-op.
	if n, err := st.RenameWallets(nil, "x"); err != nil || n != 0 {
		t.Fatalf("empty rename = (%d,%v), want (0,nil)", n, err)
	}
}
