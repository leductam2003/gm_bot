// Package api exposes the HTTP/JSON surface for the dashboard. Phase 1 covers:
// vault init/unlock, wallet CRUD (generate/import/list/delete), RPC CRUD +
// "Test All", and live balances. Mutating wallet/balance routes require an
// unlocked vault.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"zyperbot/internal/crypto"
	"zyperbot/internal/engine"
	"zyperbot/internal/events"
	"zyperbot/internal/logger"
	"zyperbot/internal/opensea"
	"zyperbot/internal/rpc"
	"zyperbot/internal/store"
	"zyperbot/internal/telegram"
)

type Server struct {
	st    *store.Store
	vault *crypto.Vault
	pool  *rpc.Pool
	eng   *engine.Engine
	log   *logger.Logger
	hub   *events.Hub
	tg    *telegram.Service
	osc   *opensea.Client

	fundCancels sync.Map // runId -> context.CancelFunc for in-flight disperse/consolidate
}

func New(st *store.Store, vault *crypto.Vault, pool *rpc.Pool, eng *engine.Engine, log *logger.Logger, hub *events.Hub, tg *telegram.Service) *Server {
	return &Server{st: st, vault: vault, pool: pool, eng: eng, log: log, hub: hub, tg: tg, osc: opensea.New()}
}

// Router wires routes and static file serving.
func (s *Server) Router(webDir string) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30_000_000_000)) // 30s

	r.Route("/api", func(r chi.Router) {
		r.Use(s.authGuard)
		r.Use(s.requestLog) // record every API call + error in the Logs tab
		r.Get("/status", s.handleStatus)
		r.Get("/home", s.handleHome)             // dashboard: realized PNL + activity
		r.Post("/home/reset", s.handleHomeReset) // clear PNL/activity history
		r.Post("/home/sync", s.handleHomeSync)   // poll OpenSea for listing-sales now
		// Vault is auto-managed (no master password) and stays unlocked for the life of
		// the process — there is no lock/unlock surface to expose.

		r.Get("/chains", s.handleChains)

		r.Get("/settings", s.handleGetSettings)
		r.Post("/settings", s.handleSetSettings)
		r.Get("/appsettings", s.handleGetAppSettings)
		r.Post("/appsettings", s.handleSetAppSettings)
		r.Post("/discord/test", s.handleDiscordTest)
		r.Get("/update/check", s.handleUpdateCheck)

		r.Get("/wallets", s.handleListWallets)
		r.Post("/wallets/generate", s.handleGenerateWallets)
		r.Post("/wallets/import", s.handleImportWallets)
		r.Delete("/wallets/{id}", s.handleDeleteWallet)
		r.Post("/wallets/reveal/{id}", s.handleRevealWallet)
		r.Post("/wallets/reveal", s.handleRevealWalletsBulk)
		r.Post("/wallets/{id}/send", s.handleSendFunds)
		r.Post("/wallets/balances", s.handleBalances)
		r.Post("/funds/move", s.handleFundsMove)     // disperse / consolidate (native + ERC-20)
		r.Post("/funds/cancel", s.handleFundsCancel) // halt an in-flight run

		r.Get("/rpc", s.handleListRPC)
		r.Post("/rpc", s.handleAddRPC)
		r.Delete("/rpc/{id}", s.handleDeleteRPC)
		r.Post("/rpc/test", s.handleTestRPC)
		r.Get("/gas", s.handleGas)

		r.Get("/proxies", s.handleListProxies)
		r.Post("/proxies", s.handleAddProxies)
		r.Delete("/proxies/{id}", s.handleDeleteProxy)
		r.Post("/proxies/test", s.handleTestProxies)

		r.Post("/whitelist/check", s.handleWhitelistCheck)

		// Tasks (engine). Mutations that send funds require an unlocked vault.
		r.Get("/tasks", s.handleListTasks)
		r.Get("/tasks/{id}", s.handleGetTask)
		r.Post("/tasks", s.handleCreateTask)
		r.Put("/tasks/{id}", s.handleUpdateTask)
		r.Delete("/tasks/{id}", s.handleDeleteTask)
		r.Post("/tasks/{id}/start", s.handleStartTask)
		r.Post("/tasks/{id}/stop", s.handleStopTask)
		r.Post("/tasks/{id}/boost", s.handleBoostTask)
		r.Post("/tasks/group/{group}/start", s.handleStartGroup)
		r.Post("/tasks/group/{group}/stop", s.handleStopGroup)

		// NFT (OpenSea SeaDrop + manager)
		r.Post("/contract/abi", s.handleFetchABI)
		r.Post("/contract/tx", s.handleTxReplay)

		r.Post("/nft/holdings", s.handleNftHoldings)
		r.Post("/nft/resolve", s.handleNftResolve)
		r.Post("/nft/resolve-link", s.handleNftResolveLink)
		r.Post("/nft/items", s.handleNftItems)
		r.Post("/nft/floor", s.handleNftFloor)
		r.Post("/nft/fees", s.handleNftFees)
		r.Post("/nft/list", s.handleNftList)
		r.Post("/nft/cancel", s.handleNftCancel)
		r.Post("/nft/accept", s.handleNftAccept)

		// Logs
		r.Get("/logs", s.handleLogsSnapshot)

		// Telegram remote-control config
		r.Get("/telegram", s.handleGetTelegram)
		r.Post("/telegram", s.handleSetTelegram)
	})

	// WebSocket for live task + log streaming (registered at the full path so it
	// sits alongside the /api group rather than under the static file server).
	r.Get("/api/ws", s.handleWS)

	// Static dashboard.
	// Serve the dashboard assets with no-cache so a rebuilt/updated UI always loads
	// (this is a local desktop app — stale cached JS/CSS after an update is a bug).
	fs := http.FileServer(http.Dir(webDir))
	r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		fs.ServeHTTP(w, req)
	}))
	return r
}

// authGuard protects the API when the server is reachable off-box. Loopback requests
// (the desktop window) always pass. For NON-loopback requests, a shared secret is
// required: set ZYPER_AUTH_TOKEN and send it as the X-Auth-Token header. If no token
// is configured, remote requests are refused outright — the dashboard never exposes
// fund-moving routes to the network by default (set a token for an intentional VPS
// deployment, ideally also behind a reverse proxy with TLS).
func (s *Server) authGuard(next http.Handler) http.Handler {
	token := os.Getenv("ZYPER_AUTH_TOKEN")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r) {
			next.ServeHTTP(w, r)
			return
		}
		if token == "" {
			writeErr(w, http.StatusForbidden, "remote API access disabled — set ZYPER_AUTH_TOKEN to enable")
			return
		}
		got := r.Header.Get("X-Auth-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "missing or invalid X-Auth-Token")
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}
