package logger

import (
	"strings"
	"testing"

	"zyperbot/internal/events"
)

func TestRedactPrivateKey(t *testing.T) {
	// A full 32-byte private key must never survive into a log line.
	key := "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	out := redact("signing with " + key)
	if strings.Contains(out, key) {
		t.Fatalf("private key leaked: %s", out)
	}
	if !strings.Contains(out, "0xac09") || !strings.Contains(out, "ff80") {
		t.Fatalf("expected shortened form, got %s", out)
	}
}

func TestRedactSignature(t *testing.T) {
	sig := "0x" + strings.Repeat("ab", 65) // 65-byte sig
	out := redact(sig)
	if strings.Contains(out, sig) {
		t.Fatalf("signature leaked")
	}
}

func TestRedactShortAddressUntouchedFully(t *testing.T) {
	// A 20-byte address (40 hex) is borderline; ensure it is shortened, not leaked whole.
	addr := "0x52908400098527886E0F7030069857D2E4169EE7"
	out := redact(addr)
	if out == addr {
		t.Fatalf("address not shortened")
	}
}

func TestRingBufferAndHubPublish(t *testing.T) {
	hub := events.NewHub()
	ch, unsub := hub.Subscribe()
	defer unsub()
	lg, err := New(t.TempDir(), hub)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.Task(INFO, "hello", 7, nil)
	select {
	case env := <-ch:
		if env.Type != "log" {
			t.Fatalf("expected log envelope, got %s", env.Type)
		}
	default:
		t.Fatal("no event published to hub")
	}
	snap := lg.Snapshot()
	if len(snap) != 1 || snap[0].Msg != "hello" || snap[0].TaskID != 7 {
		t.Fatalf("snapshot wrong: %+v", snap)
	}
}

func TestLevelFilter(t *testing.T) {
	lg, _ := New(t.TempDir(), nil)
	defer lg.Close()
	lg.SetLevel(WARN)
	lg.Task(INFO, "should be dropped", 1, nil)
	lg.Task(ERROR, "should pass", 1, nil)
	snap := lg.Snapshot()
	if len(snap) != 1 || snap[0].Level != ERROR {
		t.Fatalf("level filter failed: %+v", snap)
	}
}
