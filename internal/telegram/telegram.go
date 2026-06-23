// Package telegram lets the operator drive the bot from a Telegram chat — list and
// start/stop/boost tasks and check balances — so mints can be triggered remotely.
// It long-polls getUpdates (no public URL needed, ideal
// for a VPS) and pushes concise task done/failed notifications. Only chat IDs on the
// allowlist may issue commands.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyperbot/internal/chains"
	"zyperbot/internal/crypto"
	"zyperbot/internal/engine"
	"zyperbot/internal/events"
	"zyperbot/internal/logger"
	"zyperbot/internal/opensea"
	"zyperbot/internal/rpc"
	"zyperbot/internal/store"
	"zyperbot/internal/wallet"
)

// Config is the persisted Telegram setup (stored as JSON in settings).
type Config struct {
	Enabled      bool    `json:"enabled"`
	Token        string  `json:"token"`
	AllowedChats []int64 `json:"allowedChats"`
	Notify       string  `json:"notify"` // "summary" | "all" | "off"
}

type Service struct {
	mu      sync.Mutex
	cfg     Config
	cancel  context.CancelFunc
	running bool
	notified map[int64]string // taskID -> last terminal status notified

	eng   *engine.Engine
	vault *crypto.Vault
	st    *store.Store
	pool  *rpc.Pool
	hub   *events.Hub
	log   *logger.Logger
	hc    *http.Client
	osc   *opensea.Client
	drafts map[int64]*draft // chatID -> in-progress task config
}

func New(eng *engine.Engine, vault *crypto.Vault, st *store.Store, pool *rpc.Pool, hub *events.Hub, lg *logger.Logger) *Service {
	return &Service{
		eng: eng, vault: vault, st: st, pool: pool, hub: hub, log: lg,
		notified: map[int64]string{},
		drafts:   map[int64]*draft{},
		osc:      opensea.New(),
		hc:       &http.Client{Timeout: 65 * time.Second}, // > long-poll timeout
	}
}

// RawConfig returns the current config WITH the real token (for persistence only).
func (s *Service) RawConfig() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// MaskedConfig returns the current config with the token masked.
func (s *Service) MaskedConfig() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.Token = maskToken(c.Token)
	return c
}

func maskToken(t string) string {
	if len(t) <= 8 {
		if t == "" {
			return ""
		}
		return "********"
	}
	return t[:4] + "…" + t[len(t)-4:]
}

// Configure applies a new config and (re)starts the poller. If the incoming token
// is masked/empty, the existing token is kept.
func (s *Service) Configure(c Config) {
	s.mu.Lock()
	if c.Token == "" || strings.Contains(c.Token, "…") || c.Token == "********" {
		c.Token = s.cfg.Token
	}
	if c.Notify == "" {
		c.Notify = "summary"
	}
	s.cfg = c
	s.mu.Unlock()
	s.restart()
}

func (s *Service) restart() {
	s.stop()
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if !cfg.Enabled || cfg.Token == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.running = true
	s.mu.Unlock()
	go s.pollLoop(ctx)
	go s.notifyLoop(ctx)
	s.log.API(logger.INFO, "telegram service started", map[string]any{"chats": len(cfg.AllowedChats)})
}

func (s *Service) stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false
	s.mu.Unlock()
}

// Running reports whether the poller is active.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// --- polling ---

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private" | "group" | "supergroup" | "channel"
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Chat      tgChat `json:"chat"`
		From      struct {
			Username string `json:"username"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
	Callback *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			Username string `json:"username"`
		} `json:"from"`
		Message *struct {
			MessageID int64  `json:"message_id"`
			Chat      tgChat `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

func (s *Service) pollLoop(ctx context.Context) {
	var offset int64
	for {
		if ctx.Err() != nil {
			return
		}
		ups, err := s.getUpdates(ctx, offset)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second): // backoff on error
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Callback != nil {
				s.handleCallback(ctx, u)
				continue
			}
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			s.handle(ctx, u)
		}
	}
}

func (s *Service) token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Token
}

func (s *Service) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	payload := map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"}}
	raw, err := s.call(ctx, "getUpdates", payload)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getUpdates not ok")
	}
	return resp.Result, nil
}

func (s *Service) call(ctx context.Context, method string, payload any) ([]byte, error) {
	tok := s.token()
	if tok == "" {
		return nil, fmt.Errorf("no telegram token")
	}
	body, _ := json.Marshal(payload)
	url := "https://api.telegram.org/bot" + tok + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (s *Service) send(ctx context.Context, chatID int64, text string) {
	_, _ = s.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true})
}

func (s *Service) deleteMessage(ctx context.Context, chatID, messageID int64) {
	_, _ = s.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID})
}

func (s *Service) allowed(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cfg.AllowedChats {
		if c == chatID {
			return true
		}
	}
	return false
}

// --- command handling ---

func (s *Service) handle(ctx context.Context, u tgUpdate) {
	chatID := u.Message.Chat.ID
	if u.Message.Chat.Type != "private" {
		return // DM-only: ignore group / supergroup / channel messages
	}
	text := strings.TrimSpace(u.Message.Text)

	if !s.allowed(chatID) {
		// Reveal only the caller's own chat id so they can add it in Settings.
		s.send(ctx, chatID, fmt.Sprintf("⛔ Unauthorized.\nYour chat id: %d\nAdd it in Settings → Telegram to control the bot.", chatID))
		return
	}

	// A pasted OpenSea collection link or contract address opens the task-config panel.
	if looksLikeNftLink(text) {
		s.startTaskDraft(ctx, chatID, text)
		return
	}

	fields := strings.Fields(text)
	if len(fields) == 0 {
		return
	}
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	// strip @botname suffix (group chats)
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	args := fields[1:]

	switch cmd {
	case "start", "menu":
		s.showMenu(ctx, chatID, 0)
	case "help":
		s.send(ctx, chatID, helpText)
	case "status":
		s.send(ctx, chatID, s.statusText())
	case "tasks":
		s.send(ctx, chatID, s.tasksText())
	case "run":
		s.cmdRun(ctx, chatID, args)
	case "stop":
		s.cmdIDAction(ctx, chatID, args, "stop")
	case "boost":
		s.cmdIDAction(ctx, chatID, args, "boost")
	case "balance":
		s.cmdBalance(ctx, chatID, args)
	case "wallets":
		s.send(ctx, chatID, s.walletsText(arg0(args)))
	case "newwallets", "genwallets":
		s.cmdNewWallets(ctx, chatID, args)
	case "rpc", "rpcs":
		s.send(ctx, chatID, s.rpcText())
	case "addrpc":
		s.cmdAddRPC(ctx, chatID, args)
	case "testrpc":
		s.cmdTestRPC(ctx, chatID, args)
	default:
		s.send(ctx, chatID, "Unknown command. /help")
	}
}

func arg0(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

const helpText = `GM Sniper — remote control

/start — open the interactive menu
Paste an OpenSea link or a contract (0x…) to set up a snipe task.

Tasks
/status · /tasks
/run <id> · /stop <id> · /boost <id>

Wallets
/wallets [group] · /newwallets <count> [group]
/balance <chainId> [group]

RPC
/rpc · /addrpc <chainId> <url> · /testrpc <chainId>`

func (s *Service) statusText() string {
	sums := s.eng.Summaries()
	running := 0
	for _, t := range sums {
		if t.Status == "running" {
			running++
		}
	}
	return fmt.Sprintf("Tasks: %d (running %d)", len(sums), running)
}

func (s *Service) tasksText() string {
	sums := s.eng.Summaries()
	if len(sums) == 0 {
		return "No tasks."
	}
	var b strings.Builder
	for _, t := range sums {
		fmt.Fprintf(&b, "#%d [%s] %s chain %d — %s (%d ✓ / %d ✗ / %d ⏳)\n",
			t.ID, t.Group, t.Mode, t.ChainID, t.Status, t.Success, t.Failed, t.Running)
	}
	return b.String()
}

func (s *Service) cmdRun(ctx context.Context, chatID int64, args []string) {
	id, ok := parseID(args)
	if !ok {
		s.send(ctx, chatID, "Usage: /run <id>")
		return
	}
	if err := s.eng.Start(id); err != nil {
		s.send(ctx, chatID, "❌ "+err.Error())
		return
	}
	s.send(ctx, chatID, fmt.Sprintf("▶ Task #%d started.", id))
}

func (s *Service) cmdIDAction(ctx context.Context, chatID int64, args []string, action string) {
	id, ok := parseID(args)
	if !ok {
		s.send(ctx, chatID, "Usage: /"+action+" <id>")
		return
	}
	switch action {
	case "stop":
		s.eng.Stop(id)
		s.send(ctx, chatID, fmt.Sprintf("■ Task #%d stopped.", id))
	case "boost":
		if err := s.eng.Boost(id); err != nil {
			s.send(ctx, chatID, "❌ "+err.Error())
			return
		}
		s.send(ctx, chatID, fmt.Sprintf("⚡ Task #%d boosted.", id))
	}
}

func (s *Service) cmdBalance(ctx context.Context, chatID int64, args []string) {
	if len(args) < 1 {
		s.send(ctx, chatID, "Usage: /balance <chainId> [group]")
		return
	}
	chainID, err := strconv.Atoi(args[0])
	if err != nil {
		s.send(ctx, chatID, "Bad chainId.")
		return
	}
	group := ""
	if len(args) > 1 {
		group = args[1]
	}
	c, cerr := chains.Get(chainID)
	if cerr != nil || len(c.RPCs) == 0 {
		s.send(ctx, chatID, "No RPC for chain.")
		return
	}
	ws, _ := s.st.ListWallets()
	var addrs []string
	for _, w := range ws {
		if w.Network != "evm" {
			continue
		}
		if group != "" && w.GroupName != group {
			continue
		}
		addrs = append(addrs, w.Address)
	}
	if len(addrs) == 0 {
		s.send(ctx, chatID, "No matching wallets.")
		return
	}
	res := s.pool.Balances(ctx, c.RPCs[0], addrs)
	total := new(big.Int)
	var b strings.Builder
	fmt.Fprintf(&b, "Balances on %s (%d wallets):\n", c.Name, len(addrs))
	shown := 0
	for _, r := range res {
		if r.Err != "" {
			continue
		}
		bal, _ := new(big.Int).SetString(r.BalanceWei, 10)
		if bal != nil {
			total.Add(total, bal)
		}
		if shown < 20 {
			fmt.Fprintf(&b, "%s  %s %s\n", short(r.Address), weiToEth(bal), c.Symbol)
			shown++
		}
	}
	fmt.Fprintf(&b, "──\nTotal: %s %s", weiToEth(total), c.Symbol)
	s.send(ctx, chatID, b.String())
}

// --- wallets ---

func (s *Service) walletsText(group string) string {
	ws, err := s.st.ListWallets()
	if err != nil {
		return "error reading wallets"
	}
	groups := map[string]int{}
	total := 0
	for _, w := range ws {
		if w.Network != "evm" {
			continue
		}
		if group != "" && w.GroupName != group {
			continue
		}
		groups[w.GroupName]++
		total++
	}
	if total == 0 {
		return "No wallets."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Wallets: %d\n", total)
	for g, n := range groups {
		fmt.Fprintf(&b, "  %s: %d\n", g, n)
	}
	return b.String()
}

func (s *Service) cmdNewWallets(ctx context.Context, chatID int64, args []string) {
	if len(args) < 1 {
		s.send(ctx, chatID, "Usage: /newwallets <count> [group]")
		return
	}
	count, err := strconv.Atoi(args[0])
	if err != nil || count < 1 || count > 1000 {
		s.send(ctx, chatID, "Count must be 1–1000.")
		return
	}
	group := "main"
	if len(args) > 1 {
		group = args[1]
	}
	keys, err := wallet.GenerateN(count)
	if err != nil {
		s.send(ctx, chatID, "generate failed: "+err.Error())
		return
	}
	added := 0
	for _, k := range keys {
		enc, serr := s.vault.Seal([]byte(k.PrivKeyHex))
		if serr != nil {
			continue
		}
		if _, aerr := s.st.AddWallet(store.Wallet{Label: "wallet", Network: "evm", Address: k.Address, EncPrivKey: enc, GroupName: group}); aerr == nil {
			added++
		}
	}
	s.send(ctx, chatID, fmt.Sprintf("Generated %d wallet(s) in group %q.", added, group))
}

// --- rpc ---

func (s *Service) rpcText() string {
	es, err := s.st.ListRPC()
	if err != nil {
		return "error reading rpc"
	}
	if len(es) == 0 {
		return "No RPC endpoints. Add one: /addrpc <chainId> <url>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "RPC: %d\n", len(es))
	for _, e := range es {
		fmt.Fprintf(&b, "  #%d chain %d  %s\n", e.ID, e.ChainID, e.URL)
	}
	return b.String()
}

func (s *Service) cmdAddRPC(ctx context.Context, chatID int64, args []string) {
	if len(args) < 2 {
		s.send(ctx, chatID, "Usage: /addrpc <chainId> <url>")
		return
	}
	chainID, err := strconv.Atoi(args[0])
	if err != nil {
		s.send(ctx, chatID, "Bad chainId.")
		return
	}
	name := ""
	if c, cerr := chains.Get(chainID); cerr == nil {
		name = c.Name
	}
	if _, aerr := s.st.AddRPC(store.RPCEndpoint{Name: name, ChainID: chainID, URL: args[1], GroupName: "main"}); aerr != nil {
		s.send(ctx, chatID, "add failed: "+aerr.Error())
		return
	}
	s.send(ctx, chatID, fmt.Sprintf("Added RPC for chain %d.", chainID))
}

func (s *Service) cmdTestRPC(ctx context.Context, chatID int64, args []string) {
	if len(args) < 1 {
		s.send(ctx, chatID, "Usage: /testrpc <chainId>")
		return
	}
	chainID, err := strconv.Atoi(args[0])
	if err != nil {
		s.send(ctx, chatID, "Bad chainId.")
		return
	}
	var urls []string
	if es, _ := s.st.ListRPCByChain(chainID); len(es) > 0 {
		for _, e := range es {
			urls = append(urls, e.URL)
		}
	} else if c, cerr := chains.Get(chainID); cerr == nil {
		urls = c.RPCs
	}
	if len(urls) == 0 {
		s.send(ctx, chatID, "No RPC for that chain.")
		return
	}
	probes := s.pool.TestAll(ctx, urls)
	var b strings.Builder
	fmt.Fprintf(&b, "RPC latency (chain %d):\n", chainID)
	for _, p := range probes {
		if p.OK {
			fmt.Fprintf(&b, "  %s  %d ms\n", hostOf(p.URL), p.LatencyMs)
		} else {
			fmt.Fprintf(&b, "  %s  fail\n", hostOf(p.URL))
		}
	}
	s.send(ctx, chatID, b.String())
}

func hostOf(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i > 0 {
		return u[:i]
	}
	return u
}

// --- notifications ---

func (s *Service) notifyLoop(ctx context.Context) {
	s.mu.Lock()
	notify := s.cfg.Notify
	s.mu.Unlock()
	if notify == "off" {
		return
	}
	ch, unsub := s.hub.Subscribe()
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-ch:
			if env.Type != "task" {
				continue
			}
			s.maybeNotifyTask(ctx, env.Data)
		}
	}
}

type taskEvent struct {
	ID      int64  `json:"id"`
	Group   string `json:"group"`
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	ChainID int    `json:"chainId"`
	Wallets []struct {
		Status string `json:"status"`
	} `json:"wallets"`
}

func (s *Service) maybeNotifyTask(ctx context.Context, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	var t taskEvent
	if json.Unmarshal(raw, &t) != nil {
		return
	}
	if t.Status != "done" && t.Status != "stopped" {
		return
	}
	s.mu.Lock()
	if s.notified[t.ID] == t.Status {
		s.mu.Unlock()
		return
	}
	s.notified[t.ID] = t.Status
	chats := append([]int64(nil), s.cfg.AllowedChats...)
	s.mu.Unlock()

	success, failed := 0, 0
	for _, w := range t.Wallets {
		switch w.Status {
		case "success":
			success++
		case "failed":
			failed++
		}
	}
	icon := "✅"
	if failed > 0 {
		icon = "⚠️"
	}
	if t.Status == "stopped" {
		icon = "■"
	}
	msg := fmt.Sprintf("%s Task #%d (%s, chain %d) %s\n%d ✓ / %d ✗", icon, t.ID, t.Mode, t.ChainID, t.Status, success, failed)
	for _, c := range chats {
		s.send(ctx, c, msg)
	}
}

// --- helpers ---

func parseID(args []string) (int64, bool) {
	if len(args) < 1 {
		return 0, false
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func short(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

func weiToEth(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	f := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18))
	return f.Text('f', 4)
}
