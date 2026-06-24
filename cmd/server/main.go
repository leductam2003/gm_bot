// Command server is the zyperbot HTTP dashboard entrypoint.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"zyperbot/internal/api"
	"zyperbot/internal/chains"
	"zyperbot/internal/config"
	"zyperbot/internal/crypto"
	"zyperbot/internal/engine"
	"zyperbot/internal/events"
	"zyperbot/internal/logger"
	"zyperbot/internal/rpc"
	"zyperbot/internal/store"
	"zyperbot/internal/telegram"
)

func main() {
	baseLog := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(baseLog)

	// Resolve data paths relative to the EXECUTABLE so the app is portable and its
	// data (db, logs, .env, web) persists across launches no matter the working dir.
	base := exeDir()

	// Load local secrets (.env) and apply the operator's default ETH RPC.
	config.LoadDotEnv(env("ZYPER_ENV", filepath.Join(base, ".env")))
	if u := config.EthRPC(); u != "" {
		chains.SetRPCs(1, []string{u})
		slog.Info("default Ethereum RPC set from .env", "host", hostOnly(u))
	}

	// Desktop app: bind a RANDOM loopback port (127.0.0.1:0) so the UI is an internal
	// window transport, not a fixed website anyone can open. Set ZYPER_ADDR explicitly
	// only for an advanced headless/VPS deployment.
	addr := env("ZYPER_ADDR", "127.0.0.1:0")
	dbPath := env("ZYPER_DB", filepath.Join(base, "zyperbot.db"))
	webDir := env("ZYPER_WEB", filepath.Join(base, "web"))
	logDir := env("ZYPER_LOGS", filepath.Join(base, "logs"))

	st, err := store.Open(dbPath)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	st.PruneWLSessions(time.Now().Unix()) // drop expired whitelist session tokens

	// Apply dashboard-configured API keys (saved in the DB) over any .env values, so
	// keys set in Settings persist across restarts and config.X() picks them up.
	for _, k := range []string{"OPENSEA_API_KEY", "ETHERSCAN_API_KEY"} {
		if v, gerr := st.GetSetting("cfg." + k); gerr == nil && v != "" {
			_ = os.Setenv(k, v)
		}
	}

	pool := rpc.NewPool()
	defer pool.Close()

	hub := events.NewHub()
	lg, err := logger.New(logDir, hub)
	if err != nil {
		slog.Error("open logs", "err", err)
		os.Exit(1)
	}
	defer lg.Close()

	vault := crypto.New()
	// No master password: auto-encrypt keys at rest with a locally-stored random
	// secret (vault.key next to the db) and auto-unlock on startup.
	if err := autoUnlockVault(vault, st, filepath.Join(base, "vault.key")); err != nil {
		// autoUnlockVault self-heals a key/db mismatch, so an error here is a genuine
		// I/O failure (can't read/write vault.key or the db) — don't run locked.
		slog.Error("vault auto-unlock", "err", err)
		os.Exit(1)
	}
	eng := engine.New(st, vault, pool, lg, hub)
	if err := eng.Load(); err != nil {
		slog.Error("load tasks", "err", err)
	}

	// Telegram remote control: load persisted config and start the poller if enabled.
	tg := telegram.New(eng, vault, st, pool, hub, lg)
	if blob, gerr := st.GetSetting("telegram.config"); gerr == nil {
		var tcfg telegram.Config
		if json.Unmarshal([]byte(blob), &tcfg) == nil {
			tg.Configure(tcfg)
		}
	}

	srv := api.New(st, vault, pool, eng, lg, hub, tg)
	go srv.RunSalesSync(context.Background()) // detect listing-sales for the Home PNL

	// Listen first so a ":0" random port is resolved before we build the window URL.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	// Warn loudly if the dashboard is reachable off-box without an auth token. The API
	// (incl. fund-moving routes) refuses remote requests unless ZYPER_AUTH_TOKEN is set.
	if ta, ok := ln.Addr().(*net.TCPAddr); ok && ta.IP != nil && !ta.IP.IsLoopback() {
		if os.Getenv("ZYPER_AUTH_TOKEN") == "" {
			slog.Warn("dashboard bound to a non-loopback address with NO auth token — remote API access is REFUSED; set ZYPER_AUTH_TOKEN (and front it with a TLS reverse proxy) for intentional remote use", "addr", ln.Addr().String())
		} else {
			slog.Info("dashboard bound off-box; remote requests require the X-Auth-Token header", "addr", ln.Addr().String())
		}
	}
	httpSrv := &http.Server{
		Handler:           srv.Router(webDir),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("zyperbot listening", "addr", "http://"+ln.Addr().String(), "db", dbPath)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	guiURL := "http://" + guiHost(ln.Addr().String())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Desktop mode: open a native window pointing at the local server. On a VPS
	// (ZYPER_HEADLESS=1) or non-Windows / no display, fall back to server-only and
	// wait for a signal — the dashboard is reachable in a browser at guiURL.
	if os.Getenv("ZYPER_HEADLESS") == "1" {
		slog.Info("headless mode — open in a browser", "url", guiURL)
		<-stop
	} else {
		waitReady(guiURL, 3*time.Second)
		if !launchGUI(guiURL, "Zyper Bot") {
			slog.Info("no desktop window — open in a browser", "url", guiURL)
			<-stop
		}
		// GUI window closed -> shut down.
	}

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// guiHost turns a bind address into a loopback URL host the local window can reach
// (0.0.0.0 / empty host -> 127.0.0.1, keeping the port).
func guiHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1:8787"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// waitReady polls the server until it responds or the timeout elapses.
func waitReady(url string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := c.Get(url + "/api/status")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(80 * time.Millisecond)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// autoUnlockVault initializes (first run) and unlocks the at-rest key vault using a
// locally-stored random secret — no user master password. The secret lives in
// keyFile (0600) next to the db; keys remain AES-256-GCM encrypted in the db.
func autoUnlockVault(v *crypto.Vault, st *store.Store, keyFile string) error {
	secret, err := os.ReadFile(keyFile)
	if err != nil {
		buf := make([]byte, 32)
		if _, rerr := rand.Read(buf); rerr != nil {
			return rerr
		}
		secret = []byte(hex.EncodeToString(buf))
		if werr := os.WriteFile(keyFile, secret, 0o600); werr != nil {
			return werr
		}
	}
	pw := string(secret)
	salt, e1 := st.GetSetting("vault.salt")
	ver, e2 := st.GetSetting("vault.verifier")
	if e1 == nil && e2 == nil {
		if uerr := v.Unlock(pw, crypto.InitParams{Salt: salt, Verifier: ver}); uerr == nil {
			return nil
		}
		// The key file no longer matches the stored salt/verifier (e.g. vault.key was
		// replaced or the db was moved without it). Rather than leave the vault
		// permanently locked with no way back, re-initialize it from the current key
		// file so the app ALWAYS starts unlocked. Any wallet keys sealed with the
		// previous secret can no longer be decrypted — but they were already unreadable
		// in the locked state, so this only restores a working app.
		slog.Warn("vault key/db mismatch — re-initializing the at-rest vault; wallets sealed with the old key become unreadable")
	}
	// First run, or recovering from a mismatch: create fresh salt/verifier.
	p, ierr := crypto.Init(pw)
	if ierr != nil {
		return ierr
	}
	if serr := st.SetSetting("vault.salt", p.Salt); serr != nil {
		return serr
	}
	if serr := st.SetSetting("vault.verifier", p.Verifier); serr != nil {
		return serr
	}
	return v.Unlock(pw, p)
}

// exeDir is the directory of the running executable (falls back to "." if unknown).
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// hostOnly strips scheme + path (and any embedded token) from a URL for safe logging.
func hostOnly(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i > 0 {
		return u[:i]
	}
	return u
}
