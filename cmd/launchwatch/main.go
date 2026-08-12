// Command launchwatch polls the NOXA Fun launchpad factory on Robinhood Chain
// and sends a Telegram alert the moment token launching becomes enabled.
//
// It watches the global `launchEnabled()` (selector 0x236a4afb) flag on the
// factory contract 0xD9eC2db5f3D1b236843925949fe5bd8a3836FCcB. That is exactly
// the switch that flips the site's "Token Launching is Currently Disabled /
// PAUSED" card off. When it turns true, everyone can launch.
//
// Usage:
//
//	go run ./cmd/launchwatch -token <BOT_TOKEN>
//	  # or set TG_BOT_TOKEN in the environment
//
// Config via flags (all optional except the token):
//
//	-token     Telegram bot token (or env TG_BOT_TOKEN)
//	-chat      Telegram chat/channel id (default "@noxaupdate")
//	-interval  poll interval (default 1s)
//	-rpc       Robinhood Chain RPC URL
//	-factory   launchpad factory address
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultRPC     = "https://rpc.mainnet.chain.robinhood.com/"
	defaultFactory = "0xD9eC2db5f3D1b236843925949fe5bd8a3836FCcB"
	// keccak256("launchEnabled()")[:4]
	selLaunchEnabled = "0x236a4afb"
	launchURL        = "https://fun.noxa.fi/rh/launch"
)

func main() {
	var (
		token    = flag.String("token", os.Getenv("TG_BOT_TOKEN"), "Telegram bot token (or env TG_BOT_TOKEN)")
		chat     = flag.String("chat", envOr("TG_CHAT_ID", "@noxaupdate"), "Telegram chat/channel id")
		interval = flag.Duration("interval", time.Second, "poll interval")
		rpc      = flag.String("rpc", defaultRPC, "Robinhood Chain RPC URL")
		factory  = flag.String("factory", defaultFactory, "launchpad factory address")
		quiet    = flag.Bool("quiet", false, "do not send a startup message to Telegram")
	)
	flag.Parse()

	if strings.TrimSpace(*token) == "" {
		log.Fatal("missing Telegram bot token: pass -token or set TG_BOT_TOKEN")
	}

	tg := &telegram{token: *token, chat: *chat, http: &http.Client{Timeout: 15 * time.Second}}
	client := &rpcClient{url: *rpc, factory: *factory, http: &http.Client{Timeout: 10 * time.Second}}

	log.Printf("launchwatch starting: factory=%s interval=%s chat=%s", *factory, *interval, *chat)

	// Initial read so we know the current state and can confirm the monitor is live.
	first, err := client.launchEnabled(context.Background())
	if err != nil {
		log.Printf("initial read failed (will keep retrying): %v", err)
	} else {
		log.Printf("initial launchEnabled = %v", first)
	}

	if !*quiet {
		status := "unknown (RPC error on first read)"
		if err == nil {
			if first {
				status = "✅ ALREADY ENABLED"
			} else {
				status = "⛔ disabled (PAUSED) — will alert when it flips"
			}
		}
		msg := fmt.Sprintf("🤖 <b>NOXA launch watcher started</b>\nPolling <code>launchEnabled()</code> every %s.\nCurrent status: %s\n%s", interval.String(), status, launchURL)
		if serr := tg.send(context.Background(), msg); serr != nil {
			log.Printf("startup Telegram send failed: %v", serr)
		}
	}

	// If it's already enabled at startup, fire the alert immediately.
	if err == nil && first {
		alert(tg)
	}

	last := first
	haveLast := err == nil

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		enabled, err := client.launchEnabled(ctx)
		cancel()
		if err != nil {
			// Transient RPC/network error: log sparingly, keep last known state.
			log.Printf("poll error: %v", err)
			continue
		}

		if !haveLast {
			haveLast = true
			last = enabled
			log.Printf("state established: launchEnabled = %v", enabled)
			if enabled {
				alert(tg)
			}
			continue
		}

		if enabled != last {
			log.Printf("state change: launchEnabled %v -> %v", last, enabled)
			if enabled {
				alert(tg)
			} else {
				// Went back to paused; note it but don't spam.
				_ = tg.send(context.Background(), "⚠️ NOXA launching was turned <b>OFF</b> again (launchEnabled=false).")
			}
			last = enabled
		}
	}
}

// alert sends the "launching is live" notification, retrying a few times so a
// transient Telegram hiccup doesn't cause the one message that matters to be lost.
func alert(tg *telegram) {
	msg := fmt.Sprintf(
		"🚀🚀 <b>NOXA TOKEN LAUNCHING IS ENABLED!</b> 🚀🚀\n\n"+
			"<code>launchEnabled()</code> just flipped to <b>true</b> on the Robinhood Chain factory.\n"+
			"Launch a token now: %s",
		launchURL,
	)
	for i := 0; i < 5; i++ {
		if err := tg.send(context.Background(), msg); err != nil {
			log.Printf("ALERT send attempt %d failed: %v", i+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("ALERT sent: launching enabled")
		return
	}
	log.Printf("ALERT: exhausted retries; check Telegram/bot manually")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- Robinhood Chain RPC ---

type rpcClient struct {
	url     string
	factory string
	http    *http.Client
}

type jsonRPCReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type jsonRPCResp struct {
	Result string `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *rpcClient) launchEnabled(ctx context.Context) (bool, error) {
	reqBody := jsonRPCReq{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_call",
		Params: []any{
			map[string]string{"to": c.factory, "data": selLaunchEnabled},
			"latest",
		},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(buf))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("rpc http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out jsonRPCResp
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("decode rpc response: %w (body=%s)", err, strings.TrimSpace(string(body)))
	}
	if out.Error != nil {
		return false, fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	hex := strings.TrimPrefix(strings.TrimSpace(out.Result), "0x")
	if hex == "" {
		return false, fmt.Errorf("empty result from eth_call")
	}
	// bool: non-zero => true
	return strings.Trim(hex, "0") != "", nil
}

// --- Telegram ---

type telegram struct {
	token string
	chat  string
	http  *http.Client
}

func (t *telegram) send(ctx context.Context, text string) error {
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	form := url.Values{}
	form.Set("chat_id", t.chat)
	form.Set("text", text)
	form.Set("parse_mode", "HTML")
	form.Set("disable_web_page_preview", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
