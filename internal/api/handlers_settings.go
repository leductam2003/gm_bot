package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// POST /api/opensea/genkeys {count} — generate N free OpenSea API keys via OpenSea's
// keyless POST /auth/keys, APPEND them (deduped) to the saved OPENSEA_API_KEY, apply
// live, and return the combined list. Free keys expire in ~7 days and are rate-limited,
// so generating several lets the app rotate across them (e.g. a 200-wallet NFT fetch).
func (s *Server) handleOpenSeaGenKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Count int `json:"count"`
	}
	_ = decode(r, &body)
	n := body.Count
	if n < 1 {
		n = 1
	}
	if n > 20 { // each key allows only 30 writes/hour — don't hammer the generator
		n = 20
	}
	var added []string
	addedExp := map[string]int64{}
	lastErr := ""
	for i := 0; i < n; i++ {
		if r.Context().Err() != nil {
			break
		}
		if i > 0 { // space out calls — the generator rate-limits bursts (429)
			select {
			case <-time.After(600 * time.Millisecond):
			case <-r.Context().Done():
			}
		}
		k, exp, err := genOpenSeaKey(r.Context())
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if k != "" {
			added = append(added, k)
			addedExp[k] = exp
		}
	}
	if len(added) == 0 {
		writeErr(w, http.StatusBadGateway, "couldn't generate any key: "+lastErr)
		return
	}
	// Merge with existing keys, dedup, preserve order.
	seen := map[string]bool{}
	var all []string
	for _, k := range config.OpenSeaKeys() {
		if !seen[k] {
			seen[k] = true
			all = append(all, k)
		}
	}
	newCount := 0
	for _, k := range added {
		if !seen[k] {
			seen[k] = true
			all = append(all, k)
			newCount++
		}
	}
	joined := strings.Join(all, "\n")
	if err := s.st.SetSetting("cfg.OPENSEA_API_KEY", joined); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Setenv("OPENSEA_API_KEY", joined) // apply live (doRotating re-reads per request)
	// Record each new key's expiry, pruned to the keys still in the list (for the test button).
	expMap := s.loadKeyExp()
	for k, e := range addedExp {
		if e > 0 {
			expMap[k] = e
		}
	}
	cur := map[string]bool{}
	for _, k := range all {
		cur[k] = true
	}
	for k := range expMap {
		if !cur[k] {
			delete(expMap, k)
		}
	}
	s.saveKeyExp(expMap)
	writeJSON(w, http.StatusOK, map[string]any{"added": newCount, "total": len(all), "keys": joined})
}

// genOpenSeaKey requests one free API key from OpenSea's keyless generator, retrying a
// few times on 429 (the generator throttles bursts). Returns the key and its expiry
// (unix seconds; 0 if OpenSea didn't provide one) so the test button can prune stale keys.
func genOpenSeaKey(ctx context.Context) (string, int64, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 1500 * time.Millisecond):
			case <-ctx.Done():
				return "", 0, ctx.Err()
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.opensea.io/api/v2/auth/keys", nil)
		req.Header.Set("accept", "application/json")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		sc := resp.StatusCode
		resp.Body.Close()
		if sc == 429 {
			lastErr = fmt.Errorf("rate limited (429) — try fewer keys")
			continue
		}
		if sc >= 400 {
			return "", 0, fmt.Errorf("opensea %d", sc)
		}
		var out struct {
			APIKey    string `json:"api_key"`
			ExpiresAt string `json:"expires_at"`
		}
		if json.Unmarshal(b, &out) == nil && out.APIKey != "" {
			var exp int64
			if t, e := time.Parse(time.RFC3339, out.ExpiresAt); e == nil {
				exp = t.Unix()
			}
			return out.APIKey, exp, nil
		}
		lastErr = fmt.Errorf("no api_key in response")
	}
	return "", 0, lastErr
}

// key→expiry (unix) metadata for generated keys, so the test button can drop stale ones
// (OpenSea's read API doesn't reject expired keys, so this recorded expiry is the only
// reliable signal). Stored separately from the key list, which stays a plain newline blob.
func (s *Server) loadKeyExp() map[string]int64 {
	m := map[string]int64{}
	if v, err := s.st.GetSetting("opensea.keyexp"); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &m)
	}
	return m
}
func (s *Server) saveKeyExp(m map[string]int64) {
	b, _ := json.Marshal(m)
	_ = s.st.SetSetting("opensea.keyexp", string(b))
}
func maskKey(k string) string {
	if len(k) <= 10 {
		return k
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// POST /api/opensea/testkeys — remove keys past their recorded expiry, keeping the rest.
// OpenSea's read endpoints accept any key (they don't reject expired/invalid ones), so a
// live probe can't tell validity; the 7-day expiry recorded when a key was generated is
// the reliable signal. Keys with no recorded expiry (manually pasted) are kept — we can't
// determine them and never drop a maybe-good key.
func (s *Server) handleOpenSeaTestKeys(w http.ResponseWriter, r *http.Request) {
	keys := config.OpenSeaKeys()
	expMap := s.loadKeyExp()
	now := time.Now().Unix()
	var live []string
	removed, unknown := 0, 0
	type detail struct {
		Key    string `json:"key"`
		Status string `json:"status"`
	}
	details := []detail{}
	for _, k := range keys {
		exp, known := expMap[k]
		switch {
		case known && exp <= now:
			removed++
			delete(expMap, k)
			details = append(details, detail{maskKey(k), "expired — removed"})
		case known:
			live = append(live, k)
			days := (exp - now) / 86400
			details = append(details, detail{maskKey(k), fmt.Sprintf("valid (~%dd left)", days)})
		default:
			live = append(live, k)
			unknown++
			details = append(details, detail{maskKey(k), "kept (no expiry recorded — pasted manually)"})
		}
	}
	joined := strings.Join(live, "\n")
	if err := s.st.SetSetting("cfg.OPENSEA_API_KEY", joined); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Setenv("OPENSEA_API_KEY", joined)
	s.saveKeyExp(expMap)
	writeJSON(w, http.StatusOK, map[string]any{
		"tested": len(keys), "valid": len(live), "removed": removed, "unknown": unknown,
		"keys": joined, "details": details,
	})
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

// defaultUpdateRepo is checked when the user hasn't set their own update source, so the
// on-open / background auto-check works out of the box.
const defaultUpdateRepo = "leductam2003/gm_bot"

// GET /api/update/check — compare the running version to the latest GitHub release of
// the configured repo (app.config "updateRepo" = "owner/name"). Read-only: it never
// downloads or runs anything — just reports whether a newer build exists.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"current": config.Version, "configured": true}
	repo := s.updateRepo() // configured source, or the canonical default (auto-check works out of the box)
	body, status, err := githubGET(r.Context(), "https://api.github.com/repos/"+repo+"/releases/latest")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	if status == http.StatusOK {
		_ = json.Unmarshal(body, &rel)
	} else if status == http.StatusNotFound {
		// No published Release — the common case for a plain code repo. Fall back to the
		// newest git tag so a bare `git tag vX.Y.Z && git push --tags` is enough to drive
		// the checker (no formal GitHub Release ceremony needed).
		tag, turl := latestTag(r.Context(), repo)
		if tag == "" {
			out["hasUpdate"] = false
			out["note"] = "No releases or tags published on " + repo + " yet — push a tag like v1.3.1 to enable update checks."
			writeJSON(w, http.StatusOK, out)
			return
		}
		rel.TagName, rel.HTMLURL = tag, turl
	} else {
		writeErr(w, http.StatusBadGateway, "github "+http.StatusText(status))
		return
	}
	latest := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	out["latest"] = latest
	out["url"] = rel.HTMLURL
	out["notes"] = rel.Body
	out["hasUpdate"] = semverGreater(latest, config.Version)
	writeJSON(w, http.StatusOK, out)
}

// githubGET does one GitHub API GET, returning the body and HTTP status (or a transport error).
func githubGET(ctx context.Context, url string) ([]byte, int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// latestTag returns the semver-greatest tag of a repo (name, web URL), or "" if none.
// The /tags list order isn't semver-sorted, so pick the max ourselves.
func latestTag(ctx context.Context, repo string) (string, string) {
	body, status, err := githubGET(ctx, "https://api.github.com/repos/"+repo+"/tags?per_page=100")
	if err != nil || status != http.StatusOK {
		return "", ""
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(body, &tags) != nil || len(tags) == 0 {
		return "", ""
	}
	best := tags[0].Name
	for _, t := range tags[1:] {
		if semverGreater(strings.TrimPrefix(t.Name, "v"), strings.TrimPrefix(best, "v")) {
			best = t.Name
		}
	}
	return best, "https://github.com/" + repo + "/releases/tag/" + best
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
