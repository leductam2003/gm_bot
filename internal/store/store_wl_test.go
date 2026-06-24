package store

import (
	"path/filepath"
	"testing"
)

func TestWLSessions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// upsert + case-insensitive read.
	if err := st.SaveWLSession("0xABCdef0000000000000000000000000000000001", "tok1", 5000); err != nil {
		t.Fatal(err)
	}
	if tok, exp, ok := st.GetWLSession("0xabcDEF0000000000000000000000000000000001"); !ok || tok != "tok1" || exp != 5000 {
		t.Fatalf("get = %q %d %v, want tok1/5000/true", tok, exp, ok)
	}
	// upsert overwrites.
	_ = st.SaveWLSession("0xabcdef0000000000000000000000000000000001", "tok2", 9000)
	if tok, exp, _ := st.GetWLSession("0xabcdef0000000000000000000000000000000001"); tok != "tok2" || exp != 9000 {
		t.Fatalf("after upsert = %q %d, want tok2/9000", tok, exp)
	}
	// a second wallet that's already expired.
	_ = st.SaveWLSession("0x00000000000000000000000000000000000000aa", "old", 100)
	// prune anything expired at/before now=200 → removes the expired one, keeps tok2 (exp 9000).
	st.PruneWLSessions(200)
	if _, _, ok := st.GetWLSession("0x00000000000000000000000000000000000000aa"); ok {
		t.Fatal("expired session survived prune")
	}
	if _, _, ok := st.GetWLSession("0xabcdef0000000000000000000000000000000001"); !ok {
		t.Fatal("valid session wrongly pruned")
	}
	// delete.
	st.DeleteWLSession("0xabcdef0000000000000000000000000000000001")
	if _, _, ok := st.GetWLSession("0xabcdef0000000000000000000000000000000001"); ok {
		t.Fatal("session survived delete")
	}
}
