package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"zyperbot/internal/config"
)

// cfgKeys are the env-var-backed settings configurable from the dashboard. Saved in the
// DB under "cfg.<NAME>" and mirrored into the process env so config.X() picks them up
// live (and they survive restarts via loadDBSettings in main).
var cfgKeys = []string{"OPENSEA_API_KEY", "ETHERSCAN_API_KEY"}

// GET /api/settings — current app-configurable keys (the dashboard is loopback/token
// gated by authGuard, so returning the user's own keys to their own UI is fine).
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{}
	for _, k := range cfgKeys {
		out[k] = os.Getenv(k)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/settings {OPENSEA_API_KEY?, ETHERSCAN_API_KEY?} — persist + apply live.
// Only keys present in the body are changed; an empty string clears that key.
func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	saved := 0
	for _, k := range cfgKeys {
		v, ok := body[k]
		if !ok {
			continue
		}
		if err := s.st.SetSetting("cfg."+k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = os.Setenv(k, v) // apply to the running process immediately
		saved++
	}
	writeJSON(w, http.StatusOK, map[string]int{"saved": saved})
}

// GET /api/appsettings — the app config blob (Discord webhook, task defaults, etc.).
func (s *Server) handleGetAppSettings(w http.ResponseWriter, r *http.Request) {
	m := map[string]any{}
	if v, err := s.st.GetSetting("app.config"); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &m)
	}
	writeJSON(w, http.StatusOK, m)
}

// POST /api/appsettings {...} — merge the given keys into the app config blob.
func (s *Server) handleSetAppSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	cur := map[string]any{}
	if v, err := s.st.GetSetting("app.config"); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &cur)
	}
	for k, val := range body {
		cur[k] = val
	}
	blob, _ := json.Marshal(cur)
	if err := s.st.SetSetting("app.config", string(blob)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/update/check — compare the running version to the latest GitHub release of
// the configured repo (app.config "updateRepo" = "owner/name"). Read-only: it never
// downloads or runs anything — just reports whether a newer build exists.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"current": config.Version, "configured": false}
	repo := ""
	if v, err := s.st.GetSetting("app.config"); err == nil && v != "" {
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			if rr, ok := m["updateRepo"].(string); ok {
				repo = strings.TrimSpace(rr)
			}
		}
	}
	if repo == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["configured"] = true
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeErr(w, http.StatusBadGateway, "github "+resp.Status)
		return
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	_ = json.Unmarshal(body, &rel)
	latest := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	out["latest"] = latest
	out["url"] = rel.HTMLURL
	out["notes"] = rel.Body
	out["hasUpdate"] = semverGreater(latest, config.Version)
	writeJSON(w, http.StatusOK, out)
}

// semverGreater reports whether a > b for dotted numeric versions (e.g. "1.3.1" > "1.3.0").
func semverGreater(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, y := verPart(pa, i), verPart(pb, i)
		if x != y {
			return x > y
		}
	}
	return false
}

func verPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	num := ""
	for _, c := range parts[i] {
		if c < '0' || c > '9' {
			break
		}
		num += string(c)
	}
	n, _ := strconv.Atoi(num)
	return n
}

// POST /api/discord/test {url} — send a test message to a Discord webhook.
func (s *Server) handleDiscordTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	u := strings.TrimSpace(body.URL)
	if !strings.HasPrefix(u, "https://discord.com/api/webhooks/") && !strings.HasPrefix(u, "https://discordapp.com/api/webhooks/") {
		writeErr(w, http.StatusBadRequest, "not a Discord webhook URL")
		return
	}
	payload, _ := json.Marshal(map[string]string{"content": "✅ Zyper Bot — webhook test OK"})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		writeErr(w, http.StatusBadGateway, "discord returned "+resp.Status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
