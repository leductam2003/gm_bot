package api

import (
	"encoding/json"
	"net/http"
	"os"

	"zyperbot/internal/chains"
	"zyperbot/internal/config"
	"zyperbot/internal/logger"
	"zyperbot/internal/store"
	"zyperbot/internal/wallet"
)

// GET /api/status — version + auto-managed vault state. Informational only.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"unlocked": s.vault.Unlocked(),
		"version":  config.Version,
	})
}

// GET /api/chains — registry chains plus any user-defined custom chains (app.config
// "customChains"), so a brand-new chain can be used without a code change.
func (s *Server) handleChains(w http.ResponseWriter, r *http.Request) {
	out := chains.All()
	if v, err := s.st.GetSetting("app.config"); err == nil && v != "" {
		var m struct {
			CustomChains []struct {
				ID     int    `json:"id"`
				Name   string `json:"name"`
				Symbol string `json:"symbol"`
				RPC    string `json:"rpc"`
			} `json:"customChains"`
		}
		if json.Unmarshal([]byte(v), &m) == nil {
			seen := map[int]bool{}
			for _, c := range out {
				seen[c.ID] = true
			}
			for _, cc := range m.CustomChains {
				if cc.ID == 0 || seen[cc.ID] {
					continue
				}
				sym := cc.Symbol
				if sym == "" {
					sym = "ETH"
				}
				var rpcs []string
				if cc.RPC != "" {
					rpcs = []string{cc.RPC}
				}
				out = append(out, chains.ChainInfo{ID: cc.ID, Name: cc.Name, Symbol: sym, RPCs: rpcs})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/wallets — metadata only (no keys).
func (s *Server) handleListWallets(w http.ResponseWriter, r *http.Request) {
	ws, err := s.st.ListWallets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ws == nil {
		ws = []store.Wallet{}
	}
	writeJSON(w, http.StatusOK, ws)
}

// POST /api/wallets/generate {count,label,group}
func (s *Server) handleGenerateWallets(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Count int    `json:"count"`
		Label string `json:"label"`
		Group string `json:"group"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	keys, err := wallet.GenerateN(body.Count)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	added, err := s.persistKeys(keys, body.Label, body.Group)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added})
}

// POST /api/wallets/import {privKeys:[],group}
func (s *Server) handleImportWallets(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PrivKeys []string `json:"privKeys"`
		Group    string   `json:"group"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	var keys []wallet.Key
	for _, pk := range body.PrivKeys {
		if pk == "" {
			continue
		}
		k, err := wallet.Import(pk)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid private key in list")
			return
		}
		keys = append(keys, k)
	}
	added, err := s.persistKeys(keys, "", body.Group)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added})
}

// persistKeys seals each private key with the vault and inserts the wallet rows.
func (s *Server) persistKeys(keys []wallet.Key, label, group string) (int, error) {
	if group == "" {
		group = "default"
	}
	added := 0
	for i, k := range keys {
		enc, err := s.vault.Seal([]byte(k.PrivKeyHex))
		if err != nil {
			return added, err
		}
		lbl := label
		if lbl == "" {
			lbl = "wallet"
		}
		_ = i
		if _, err := s.st.AddWallet(store.Wallet{
			Label: lbl, Network: "evm", Address: k.Address, EncPrivKey: enc, GroupName: group,
		}); err != nil {
			// Skip duplicates (UNIQUE constraint) but keep going.
			continue
		}
		added++
	}
	return added, nil
}

// DELETE /api/wallets/{id}
func (s *Server) handleDeleteWallet(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.st.DeleteWallet(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/wallets/reveal/{id} {confirm:true} — decrypt and return ONE key.
// Disabled by default: it returns a plaintext private key in the HTTP body, which
// is inherently dangerous. Enable explicitly with ZYPER_ALLOW_REVEAL=1, behind TLS.
// Every reveal is audit-logged (id + remote addr). Phase 6 will add 2FA on top.
func (s *Server) handleRevealWallet(w http.ResponseWriter, r *http.Request) {
	if v := os.Getenv("ZYPER_ALLOW_REVEAL"); v != "1" && v != "true" {
		writeErr(w, http.StatusForbidden, "key reveal is disabled; set ZYPER_ALLOW_REVEAL=1 to enable")
		return
	}
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct{ Confirm bool `json:"confirm"` }
	_ = decode(r, &body)
	if !body.Confirm {
		writeErr(w, http.StatusBadRequest, `must pass {"confirm":true}`)
		return
	}
	wlt, err := s.st.GetWallet(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "wallet not found")
		return
	}
	// Audit BEFORE returning the secret so the access is recorded even on a crash.
	s.log.API(logger.WARN, "PRIVATE KEY REVEALED", map[string]any{
		"walletId": id, "address": wlt.Address, "remote": r.RemoteAddr,
	})
	pk, err := s.vault.Open(wlt.EncPrivKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "decrypt failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"address": wlt.Address, "privKey": string(pk)})
}
