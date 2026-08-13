package api

import (
	"bytes"
	"context"
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

// defaultUpdateRepo is checked when the user hasn't set their own update source, so the
// on-open / background auto-check works out of the box.
const defaultUpdateRepo = "leductam2003/gm_bot"

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
		repo = defaultUpdateRepo // unset → check the canonical repo so auto-check works out of the box
	}
	out["configured"] = true
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
