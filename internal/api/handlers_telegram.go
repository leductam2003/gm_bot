package api

import (
	"encoding/json"
	"net/http"

	"zyperbot/internal/telegram"
)

const telegramConfigKey = "telegram.config"

// GET /api/telegram — masked config + running state.
func (s *Server) handleGetTelegram(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"config":  s.tg.MaskedConfig(),
		"running": s.tg.Running(),
	})
}

// POST /api/telegram {Config} — persist + (re)start the poller. Requires unlock.
func (s *Server) handleSetTelegram(w http.ResponseWriter, r *http.Request) {
	var cfg telegram.Config
	if err := decode(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// Apply (Configure keeps the existing token if the incoming one is masked/empty).
	s.tg.Configure(cfg)

	// Persist the effective config (with the real token) for next boot.
	effective := s.tg.RawConfig()
	blob, _ := json.Marshal(effective)
	if err := s.st.SetSetting(telegramConfigKey, string(blob)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": s.tg.Running()})
}
