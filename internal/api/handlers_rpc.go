package api

import (
	"net/http"
	"strconv"

	"zyperbot/internal/chains"
	"zyperbot/internal/store"
)

// GET /api/rpc — all endpoints.
func (s *Server) handleListRPC(w http.ResponseWriter, r *http.Request) {
	es, err := s.st.ListRPC()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if es == nil {
		es = []store.RPCEndpoint{}
	}
	writeJSON(w, http.StatusOK, es)
}

// POST /api/rpc {name,chainId,url,group}
func (s *Server) handleAddRPC(w http.ResponseWriter, r *http.Request) {
	var e store.RPCEndpoint
	if err := decode(r, &e); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if e.URL == "" || e.ChainID == 0 {
		writeErr(w, http.StatusBadRequest, "url and chainId required")
		return
	}
	if e.GroupName == "" {
		e.GroupName = "main"
	}
	if e.Name == "" {
		if c, err := chains.Get(e.ChainID); err == nil {
			e.Name = c.Name
		}
	}
	id, err := s.st.AddRPC(e)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.ID = id
	writeJSON(w, http.StatusOK, e)
}

// DELETE /api/rpc/{id}
func (s *Server) handleDeleteRPC(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.st.DeleteRPC(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/rpc/test {urls:[]} | {chainId} — latency probe ("Test All").
func (s *Server) handleTestRPC(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URLs    []string `json:"urls"`
		ChainID int      `json:"chainId"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	urls := body.URLs
	if len(urls) == 0 && body.ChainID != 0 {
		es, err := s.st.ListRPCByChain(body.ChainID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, e := range es {
			urls = append(urls, e.URL)
		}
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "no urls to test")
		return
	}
	writeJSON(w, http.StatusOK, s.pool.TestAll(r.Context(), urls))
}

// GET /api/gas?chainId=1 — latest base fee in gwei for the UI ticker.
func (s *Server) handleGas(w http.ResponseWriter, r *http.Request) {
	chainID := 1
	if v := r.URL.Query().Get("chainId"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			chainID = n
		}
	}
	url := ""
	if es, _ := s.st.ListRPCByChain(chainID); len(es) > 0 {
		url = es[0].URL
	} else if c, err := chains.Get(chainID); err == nil && len(c.RPCs) > 0 {
		url = c.RPCs[0]
	}
	if url == "" {
		writeErr(w, http.StatusBadRequest, "no rpc")
		return
	}
	g, err := s.pool.BaseFeeGwei(r.Context(), url)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"gwei": strconv.FormatFloat(g, 'f', 2, 64)})
}

// POST /api/wallets/balances {chainId, rpcUrl?, group?} — live native balances.
func (s *Server) handleBalances(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChainID int    `json:"chainId"`
		RPCUrl  string `json:"rpcUrl"`
		Group   string `json:"group"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	url := body.RPCUrl
	if url == "" {
		// Fall back to the first configured RPC for the chain, else the registry default.
		es, _ := s.st.ListRPCByChain(body.ChainID)
		if len(es) > 0 {
			url = es[0].URL
		} else if c, err := chains.Get(body.ChainID); err == nil && len(c.RPCs) > 0 {
			url = c.RPCs[0]
		}
	}
	if url == "" {
		writeErr(w, http.StatusBadRequest, "no rpc url for chain")
		return
	}
	ws, err := s.st.ListWallets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var addrs []string
	for _, wlt := range ws {
		if wlt.Network != "evm" {
			continue
		}
		if body.Group != "" && wlt.GroupName != body.Group {
			continue
		}
		addrs = append(addrs, wlt.Address)
	}
	if len(addrs) == 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.pool.Balances(r.Context(), url, addrs))
}
