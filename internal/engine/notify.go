package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// notifyDiscord posts a one-line task summary to the configured Discord webhook (set
// in Settings → APP). Fire-and-forget; never blocks or fails the task.
func (e *Engine) notifyDiscord(rt *TaskRuntime, cfg TaskConfig, final string) {
	url, _ := e.appConfig()["discordWebhook"].(string)
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "https://discord.com/api/webhooks/") && !strings.HasPrefix(url, "https://discordapp.com/api/webhooks/") {
		return
	}
	rt.mu.Lock()
	var ok, fail int
	for _, w := range rt.Wallets {
		switch w.Status {
		case "success":
			ok++
		case "failed":
			fail++
		}
	}
	rt.mu.Unlock()
	emoji := "✅"
	if fail > 0 && ok > 0 {
		emoji = "⚠️"
	} else if fail > 0 {
		emoji = "❌"
	}
	content := fmt.Sprintf("%s Task #%d (%s, chain %d) %s — %d ok, %d failed", emoji, cfg.ID, cfg.Mode, cfg.ChainID, final, ok, fail)
	payload, _ := json.Marshal(map[string]string{"content": content})
	go func() {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("content-type", "application/json")
		if resp, derr := (&http.Client{Timeout: 8 * time.Second}).Do(req); derr == nil {
			resp.Body.Close()
		}
	}()
}
