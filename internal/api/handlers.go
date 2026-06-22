package api

import (
	"encoding/json"
	"net/http"
	"os"

	"zyperbot/internal/chains"
	"zyperbot/internal/config"
	"zyperbot/internal/crypto"
	"zyperbot/internal/logger"
	"zyperbot/internal/store"
	"zyperbot/internal/wallet"
)

const vaultSaltKey = "vault.salt"
const vaultVerifierKey = "vault.verifier"

// GET /api/status — vault state + counts, so the UI knows what screen to show.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	_, errSalt := s.st.GetSetting(vaultSaltKey)
	initialized := errSalt == nil
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": initialized,
		"unlocked":    s.vault.Unlocked(),
		"version":     config.Version,
	})
}

// POST /api/vault/init {password} — first-run; create salt+verifier.
func (s *Server) handleVaultInit(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string `json:"password"` }
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if _, err := s.st.GetSetting(vaultSaltKey); err == nil {
		writeErr(w, http.StatusConflict, "vault already initialized")
		return
	}
	p, err := crypto.Init(body.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SetSetting(vaultSaltKey, p.Salt); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.st.SetSetting(vaultVerifierKey, p.Verifier); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.vault.Unlock(body.Password, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/vault/unlock {password}
func (s *Server) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string `json:"password"` }
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	salt, err1 := s.st.GetSetting(vaultSaltKey)
	ver, err2 := s.st.GetSetting(vaultVerifierKey)
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "vault not initialized")
		return
	}
	if err := s.vault.Unlock(body.Password, crypto.InitParams{Salt: salt, Verifier: ver}); err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/vault/lock
func (s *Server) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	s.vault.Lock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
