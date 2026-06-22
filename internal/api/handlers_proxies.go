package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zyperbot/internal/store"
)

// normalizeProxy turns one input line into a proxy URL, or ("", false) to skip.
// Accepts: full URL (http/https/socks5://[user:pass@]host:port), host:port,
// or host:port:user:pass (the common residential-proxy format).
func normalizeProxy(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	if strings.Contains(line, "://") {
		if u, err := url.Parse(line); err == nil && u.Host != "" {
			return u.String(), true
		}
		return "", false
	}
	p := strings.Split(line, ":")
	u := &url.URL{Scheme: "http"}
	switch len(p) {
	case 2: // host:port
		u.Host = p[0] + ":" + p[1]
	case 4: // host:port:user:pass
		u.Host = p[0] + ":" + p[1]
		u.User = url.UserPassword(p[2], p[3])
	default:
		return "", false
	}
	return u.String(), true
}

// GET /api/proxies
func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.ListProxies()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ps == nil {
		ps = []store.Proxy{}
	}
	writeJSON(w, http.StatusOK, ps)
}

// POST /api/proxies {lines, group} — add one proxy per parseable line.
func (s *Server) handleAddProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lines string `json:"lines"`
		Group string `json:"group"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	group := strings.TrimSpace(body.Group)
	if group == "" {
		group = "default"
	}
	added := 0
	for _, ln := range strings.Split(body.Lines, "\n") {
		u, ok := normalizeProxy(ln)
		if !ok {
			continue
		}
		if _, err := s.st.AddProxy(store.Proxy{URL: u, GroupName: group}); err == nil {
			added++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added})
}

// DELETE /api/proxies/{id}
func (s *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.st.DeleteProxy(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type proxyTestResult struct {
	URL   string `json:"url"`
	OK    bool   `json:"ok"`
	MS    int64  `json:"ms"`
	IP    string `json:"ip,omitempty"`
	Error string `json:"error,omitempty"`
}

// POST /api/proxies/test {urls} — check each proxy reaches the internet (returns the
// egress IP + latency). Runs concurrently.
func (s *Server) handleTestProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URLs []string `json:"urls"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	out := make([]proxyTestResult, len(body.URLs))
	var wg sync.WaitGroup
	for i, u := range body.URLs {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			out[i] = testProxy(r.Context(), u)
		}(i, u)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

func testProxy(ctx context.Context, proxyURL string) proxyTestResult {
	res := proxyTestResult{URL: proxyURL}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		res.Error = "bad proxy url"
		return res
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		Timeout:   8 * time.Second,
	}
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	resp, err := client.Do(req)
	if err != nil {
		res.Error = trimErr(err.Error())
		return res
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	res.OK = resp.StatusCode < 400
	res.MS = time.Since(start).Milliseconds()
	res.IP = strings.TrimSpace(string(b))
	if !res.OK {
		res.Error = resp.Status
	}
	return res
}

func trimErr(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
