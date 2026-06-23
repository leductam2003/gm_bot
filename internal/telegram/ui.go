// Interactive inline-keyboard UI for the Telegram bot (DM-only). Pasting an OpenSea
// collection link or a contract address opens a task-config panel (mode, wallet group,
// quantity) that creates a snipe task. A main menu lists tasks with run/stop toggles.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"zyperbot/internal/logger"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"

	"zyperbot/internal/chains"
	"zyperbot/internal/engine"
	"zyperbot/internal/evm"
)

// draft is the in-progress task config for one chat (built from a pasted link/hash).
type draft struct {
	chainID      int
	contract     string
	name         string
	seadrop      bool
	priceWei     string
	maxPerWallet int
	hexMode      bool   // replay: send rawHex calldata + value as-is
	rawHex       string // calldata copied from a replayed tx
	valueWei     string // value copied from a replayed tx
	mode         string // "simulate" | "action"
	group        string // wallet group; "" = all wallets
	quantity     int
	panelMsgID   int64
}

func (s *Service) getDraft(chatID int64) *draft {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drafts[chatID]
}
func (s *Service) setDraft(chatID int64, d *draft) {
	s.mu.Lock()
	s.drafts[chatID] = d
	s.mu.Unlock()
}
func (s *Service) clearDraft(chatID int64) {
	s.mu.Lock()
	delete(s.drafts, chatID)
	s.mu.Unlock()
}

// --- inline keyboards ---

type ikButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

func (s *Service) sendKB(ctx context.Context, chatID int64, text string, rows [][]ikButton) int64 {
	payload := map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true}
	if len(rows) > 0 {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	raw, err := s.call(ctx, "sendMessage", payload)
	if err != nil {
		return 0
	}
	var resp struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &resp)
	return resp.Result.MessageID
}

func (s *Service) editKB(ctx context.Context, chatID, msgID int64, text string, rows [][]ikButton) {
	payload := map[string]any{"chat_id": chatID, "message_id": msgID, "text": text, "disable_web_page_preview": true}
	if len(rows) > 0 {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	_, _ = s.call(ctx, "editMessageText", payload)
}

func (s *Service) answerCallback(ctx context.Context, id, text string) {
	_, _ = s.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text})
}

var backToMenu = [][]ikButton{{{Text: "‹ Menu", Data: "menu"}}}

// --- main menu ---

func (s *Service) showMenu(ctx context.Context, chatID, editMsgID int64) {
	sums := s.eng.Summaries()
	running := 0
	for _, t := range sums {
		if t.Status == "running" {
			running++
		}
	}
	ws, _ := s.st.ListWallets()
	wcount := 0
	for _, w := range ws {
		if w.Network == "evm" {
			wcount++
		}
	}
	head := fmt.Sprintf("🎯 GM Sniper — Tasks\n\nTasks: %d   |   running: %d\nWallets: %d", len(sums), running, wcount)

	var rows [][]ikButton
	for _, t := range sums {
		icon := "▶️"
		if t.Status == "running" {
			icon = "⏹"
		}
		label := fmt.Sprintf("#%d %s · %s (%d✓/%d✗)", t.ID, t.Mode, t.Status, t.Success, t.Failed)
		rows = append(rows, []ikButton{
			{Text: icon, Data: fmt.Sprintf("arm:%d", t.ID)},
			{Text: label, Data: fmt.Sprintf("task:%d", t.ID)},
		})
	}
	rows = append(rows, []ikButton{{Text: "➕ New Task", Data: "new"}})
	rows = append(rows, []ikButton{{Text: "👛 Wallets", Data: "wallets"}, {Text: "🔄 Refresh", Data: "refresh"}})

	if editMsgID != 0 {
		s.editKB(ctx, chatID, editMsgID, head, rows)
	} else {
		s.sendKB(ctx, chatID, head, rows)
	}
}

func (s *Service) showWallets(ctx context.Context, chatID, msgID int64) {
	ws, _ := s.st.ListWallets()
	groups := map[string]int{}
	total := 0
	for _, w := range ws {
		if w.Network != "evm" {
			continue
		}
		groups[w.GroupName]++
		total++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "👛 Wallets: %d\n", total)
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		fmt.Fprintf(&b, "  • %s: %d\n", g, groups[g])
	}
	if total == 0 {
		b.WriteString("(none — add wallets in the desktop app)")
	}
	s.editKB(ctx, chatID, msgID, b.String(), backToMenu)
}

func (s *Service) showTask(ctx context.Context, chatID, msgID int64, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	var sum *engine.TaskSummary
	for _, t := range s.eng.Summaries() {
		if t.ID == id {
			tt := t
			sum = &tt
			break
		}
	}
	if sum == nil {
		s.showMenu(ctx, chatID, msgID)
		return
	}
	txt := fmt.Sprintf("Task #%d\nGroup: %s\nMode: %s\nChain: %d\nStatus: %s\n%d ✓ / %d ✗ / %d ⏳",
		sum.ID, sum.Group, sum.Mode, sum.ChainID, sum.Status, sum.Success, sum.Failed, sum.Running)
	arm := "▶️ Run"
	if sum.Status == "running" {
		arm = "⏹ Stop"
	}
	rows := [][]ikButton{
		{{Text: arm, Data: fmt.Sprintf("arm:%d", id)}, {Text: "⚡ Boost", Data: fmt.Sprintf("boost:%d", id)}},
		{{Text: "🗑 Delete", Data: fmt.Sprintf("del:%d", id)}},
		{{Text: "‹ Back", Data: "menu"}},
	}
	s.editKB(ctx, chatID, msgID, txt, rows)
}

func (s *Service) toggleArm(ctx context.Context, chatID, msgID int64, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	running := false
	for _, t := range s.eng.Summaries() {
		if t.ID == id {
			running = t.Status == "running"
			break
		}
	}
	if running {
		s.eng.Stop(id)
	} else if err := s.eng.Start(id); err != nil {
		s.send(ctx, chatID, "❌ "+err.Error())
	}
	s.showMenu(ctx, chatID, msgID)
}

// --- task config panel ---

func groupLabel(g string) string {
	if g == "" {
		return "All wallets"
	}
	return g
}

func (s *Service) configText(d *draft) string {
	name := d.name
	if name == "" {
		name = short(d.contract)
	}
	if d.hexMode {
		val := "0"
		if pw, ok := new(big.Int).SetString(d.valueWei, 10); ok {
			val = weiToEth(pw)
		}
		txt := fmt.Sprintf("⚙️ New Task — Replay Tx\n\n%s\nChain: %d\nContract: %s\nMode: %s\nWallets: %s\nValue: %s ETH\nCalldata: %s",
			name, d.chainID, short(d.contract), d.mode, groupLabel(d.group), val, shortHex(d.rawHex))
		txt += "\n\n⚠ Re-sends the raw calldata as-is. If the tx hard-codes a recipient/minter, it may not go to your wallet."
		return txt
	}
	kind := "Contract"
	if d.seadrop {
		kind = "SeaDrop"
	}
	txt := fmt.Sprintf("⚙️ New Task — %s\n\nChain: %d\nContract: %s\nType: %s\nMode: %s\nWallets: %s\nQuantity: %d",
		name, d.chainID, short(d.contract), kind, d.mode, groupLabel(d.group), d.quantity)
	if d.seadrop && d.priceWei != "" && d.priceWei != "0" {
		if pw, ok := new(big.Int).SetString(d.priceWei, 10); ok {
			txt += "\nPrice/unit: " + weiToEth(pw) + " (on-chain)"
		}
	}
	if !d.seadrop {
		txt += "\n\n⚠ Not a SeaDrop collection — after creating, set the mint function for it in the desktop app."
	}
	return txt
}

func shortHex(h string) string {
	if len(h) <= 22 {
		return h
	}
	return h[:12] + "…" + h[len(h)-6:]
}

func (s *Service) configRows(d *draft) [][]ikButton {
	rows := [][]ikButton{
		{{Text: "Mode: " + d.mode, Data: "cfg:mode"}},
		{{Text: "Wallets: " + groupLabel(d.group), Data: "cfg:grp"}},
	}
	if !d.hexMode { // quantity applies to mints, not a raw replay
		rows = append(rows, []ikButton{{Text: "− qty", Data: "cfg:q-"}, {Text: fmt.Sprintf("Qty: %d", d.quantity), Data: "cfg:noop"}, {Text: "+ qty", Data: "cfg:q+"}})
	}
	rows = append(rows,
		[]ikButton{{Text: "✅ Create Task", Data: "cfg:create"}},
		[]ikButton{{Text: "✖ Cancel", Data: "cfg:cancel"}},
	)
	return rows
}

func (s *Service) renderConfig(ctx context.Context, chatID, msgID int64, d *draft) {
	s.editKB(ctx, chatID, msgID, s.configText(d), s.configRows(d))
}

// startTaskDraft resolves a pasted link/address and opens the config panel.
func (s *Service) startTaskDraft(ctx context.Context, chatID int64, text string) {
	mid := s.sendKB(ctx, chatID, "🔎 Resolving…", nil)
	d, err := s.resolveInput(ctx, text)
	if err != nil {
		s.editKB(ctx, chatID, mid, "❌ "+err.Error(), backToMenu)
		return
	}
	d.panelMsgID = mid
	s.setDraft(chatID, d)
	s.renderConfig(ctx, chatID, mid, d)
}

func (s *Service) configCallback(ctx context.Context, chatID, msgID int64, action string) {
	d := s.getDraft(chatID)
	if d == nil {
		s.showMenu(ctx, chatID, msgID)
		return
	}
	switch action {
	case "mode":
		if d.mode == "simulate" {
			d.mode = "action"
		} else {
			d.mode = "simulate"
		}
	case "grp":
		d.group = s.nextGroup(d.group)
	case "q+":
		d.quantity++
	case "q-":
		if d.quantity > 1 {
			d.quantity--
		}
	case "noop":
		return
	case "cancel":
		s.clearDraft(chatID)
		s.showMenu(ctx, chatID, msgID)
		return
	case "create":
		s.createFromDraft(ctx, chatID, msgID, d)
		return
	}
	s.renderConfig(ctx, chatID, msgID, d)
}

func (s *Service) createFromDraft(ctx context.Context, chatID, msgID int64, d *draft) {
	group := d.group
	if group == "" {
		group = "default"
	}
	cfg := engine.TaskConfig{
		ChainID:         d.chainID,
		ContractAddress: d.contract,
		Mode:            engine.Mode(d.mode),
		Group:           group,
		WalletIDs:       s.walletIDsForGroup(d.group), // nil = all evm wallets
		Gas:             evm.GasParams{Mode: evm.GasAuto},
	}
	switch {
	case d.hexMode:
		cfg.HexMode = true
		cfg.RawHex = d.rawHex
		cfg.ValueWei = d.valueWei
	case d.seadrop:
		cfg.Seadrop = true
		cfg.Quantity = d.quantity
		cfg.MintPriceWei = d.priceWei
	}
	id, err := s.eng.Create(cfg)
	if err != nil {
		s.editKB(ctx, chatID, msgID, "❌ create failed: "+err.Error(), backToMenu)
		return
	}
	s.clearDraft(chatID)
	s.log.API(logger.INFO, "telegram task created", map[string]any{"taskId": id, "chain": d.chainID, "mode": d.mode})
	s.editKB(ctx, chatID, msgID,
		fmt.Sprintf("✅ Task #%d created (%s · chain %d · %s).\nTap ▶️ in the menu to arm it.", id, d.mode, d.chainID, groupLabel(d.group)),
		backToMenu)
}

// --- callback router ---

func (s *Service) handleCallback(ctx context.Context, u tgUpdate) {
	cb := u.Callback
	if cb == nil || cb.Message == nil {
		return
	}
	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID
	if cb.Message.Chat.Type != "private" { // DM-only
		s.answerCallback(ctx, cb.ID, "")
		return
	}
	if !s.allowed(chatID) {
		s.answerCallback(ctx, cb.ID, "Unauthorized")
		return
	}
	s.answerCallback(ctx, cb.ID, "") // ack so the spinner stops
	data := cb.Data

	switch {
	case data == "menu" || data == "refresh":
		s.showMenu(ctx, chatID, msgID)
	case data == "new":
		s.editKB(ctx, chatID, msgID,
			"➕ New Task\n\nPaste an OpenSea collection link, or a contract address (0x…), to set up a snipe task.",
			backToMenu)
	case data == "wallets":
		s.showWallets(ctx, chatID, msgID)
	case strings.HasPrefix(data, "arm:"):
		s.toggleArm(ctx, chatID, msgID, data[len("arm:"):])
	case strings.HasPrefix(data, "task:"):
		s.showTask(ctx, chatID, msgID, data[len("task:"):])
	case strings.HasPrefix(data, "boost:"):
		if id, e := strconv.ParseInt(data[len("boost:"):], 10, 64); e == nil {
			_ = s.eng.Boost(id)
		}
		s.showMenu(ctx, chatID, msgID)
	case strings.HasPrefix(data, "del:"):
		if id, e := strconv.ParseInt(data[len("del:"):], 10, 64); e == nil {
			_ = s.eng.Delete(id)
		}
		s.showMenu(ctx, chatID, msgID)
	case strings.HasPrefix(data, "cfg:"):
		s.configCallback(ctx, chatID, msgID, data[len("cfg:"):])
	}
}

// --- link resolution (mirrors the dashboard's /nft/resolve-link) ---

// looksLikeTaskInput reports whether a plain message should open the task panel:
// an OpenSea link, an explorer (etherscan-family) link, a contract address, or a tx hash.
func looksLikeTaskInput(text string) bool {
	t := strings.TrimSpace(text)
	if common.IsHexAddress(t) || isTxHash(t) {
		return true
	}
	low := strings.ToLower(t)
	if strings.Contains(low, "opensea.io") {
		return true
	}
	return explorerChainOf(low) != 0
}

func isTxHash(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// explorerChains maps an explorer domain to its chainId (etherscan family).
var explorerChains = map[string]int{
	"etherscan.io": 1, "polygonscan.com": 137, "basescan.org": 8453,
	"optimistic.etherscan.io": 10, "bscscan.com": 56, "lineascan.build": 59144,
	"arbiscan.io": 42161, "sepolia.etherscan.io": 11155111,
}

// explorerChainOf returns the chainId for the longest explorer domain found in low
// (so "sepolia.etherscan.io" beats "etherscan.io"), or 0 if none.
func explorerChainOf(low string) int {
	best, bestLen := 0, 0
	for dom, id := range explorerChains {
		if len(dom) > bestLen && strings.Contains(low, dom) {
			best, bestLen = id, len(dom)
		}
	}
	return best
}

// parseExplorerURL extracts a tx hash or address from an explorer URL.
func parseExplorerURL(text string) (kind, value string, chainID int, ok bool) {
	chainID = explorerChainOf(strings.ToLower(text))
	if chainID == 0 {
		return "", "", 0, false
	}
	u := text
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	parts := strings.Split(strings.Trim(u, "/"), "/")
	for i, p := range parts {
		lp := strings.ToLower(p)
		if lp == "tx" && i+1 < len(parts) && isTxHash(parts[i+1]) {
			return "tx", parts[i+1], chainID, true
		}
		if (lp == "address" || lp == "token") && i+1 < len(parts) && common.IsHexAddress(parts[i+1]) {
			return "addr", parts[i+1], chainID, true
		}
	}
	return "", "", chainID, false
}

// parseNftLink extracts a contract / collection-slug / chain from a raw address, an
// OpenSea collection or item URL, or a bare slug. Pure (no network) for testability.
func parseNftLink(text string) (contract, slug string, chainID int) {
	v := strings.TrimSpace(text)
	chainID = 1
	switch {
	case common.IsHexAddress(v):
		contract = v
	case strings.Contains(strings.ToLower(v), "opensea.io"):
		u := v
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			u = u[:i]
		}
		parts := strings.Split(strings.Trim(u, "/"), "/")
		for i, p := range parts {
			lp := strings.ToLower(p)
			if lp == "collection" && i+1 < len(parts) {
				slug = parts[i+1]
				break
			}
			if (lp == "assets" || lp == "item") && i+2 < len(parts) {
				if id, ok := chains.ChainIDFromSlug(parts[i+1]); ok {
					chainID = id
				}
				if common.IsHexAddress(parts[i+2]) {
					contract = parts[i+2]
				}
				break
			}
		}
	default:
		slug = v
	}
	return contract, slug, chainID
}

// resolveInput dispatches a pasted message to the right resolver: tx hash / explorer
// /tx/ link -> replay; explorer /address|token/ link -> contract; else OpenSea/contract.
func (s *Service) resolveInput(ctx context.Context, text string) (*draft, error) {
	t := strings.TrimSpace(text)
	if isTxHash(t) {
		return s.resolveTx(ctx, t, 1)
	}
	if kind, val, chainID, ok := parseExplorerURL(t); ok {
		if kind == "tx" {
			return s.resolveTx(ctx, val, chainID)
		}
		return s.resolveContract(ctx, val, chainID, "")
	}
	return s.resolveLink(ctx, t)
}

func (s *Service) resolveLink(ctx context.Context, text string) (*draft, error) {
	contract, slug, chainID := parseNftLink(text)
	name := ""
	if contract == "" && slug != "" {
		if !s.osc.HasKey() {
			return nil, errors.New("OpenSea API key not set — add it in Settings to resolve collection links")
		}
		info, err := s.osc.Collection(ctx, slug)
		if err != nil {
			return nil, fmt.Errorf("resolve failed: %w", err)
		}
		contract, name = info.Contract, info.Name
		if id, ok := chains.ChainIDFromSlug(info.Chain); ok {
			chainID = id
		}
	}
	return s.resolveContract(ctx, contract, chainID, name)
}

// resolveContract builds a mint/contract draft (detects SeaDrop + collection name).
func (s *Service) resolveContract(ctx context.Context, contract string, chainID int, name string) (*draft, error) {
	if !common.IsHexAddress(contract) {
		return nil, errors.New("couldn't find a contract for that")
	}
	d := &draft{chainID: chainID, contract: common.HexToAddress(contract).Hex(), name: name, mode: "simulate", quantity: 1}
	if client, err := s.clientForChain(ctx, chainID); err == nil {
		cAddr := common.HexToAddress(contract)
		if d.name == "" {
			d.name = evm.CollectionName(ctx, client, cAddr)
		}
		if evm.IsSeaDropMintable(ctx, client, cAddr) {
			if res, e := evm.ResolveSeaDrop(ctx, client, cAddr, 1, common.Address{}); e == nil {
				d.seadrop = true
				d.priceWei = res.Drop.MintPrice.String()
				d.maxPerWallet = int(res.Drop.MaxTotalMintableByWallet)
			}
		}
	}
	return d, nil
}

// resolveTx builds a replay draft from a tx hash — copies its target, calldata, and value
// so each of the user's wallets re-sends the same transaction.
func (s *Service) resolveTx(ctx context.Context, hash string, chainID int) (*draft, error) {
	client, err := s.clientForChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	tx, _, terr := client.TransactionByHash(ctx, common.HexToHash(hash))
	if terr != nil {
		return nil, fmt.Errorf("tx not found on chain %d — paste the matching explorer link", chainID)
	}
	if tx.To() == nil {
		return nil, errors.New("that tx is a contract creation — nothing to replay")
	}
	to := *tx.To()
	d := &draft{
		chainID:  chainID,
		contract: to.Hex(),
		hexMode:  true,
		rawHex:   hexutil.Encode(tx.Data()),
		valueWei: tx.Value().String(),
		mode:     "simulate",
		quantity: 1,
		name:     "Replay " + short(hash),
	}
	if cn := evm.CollectionName(ctx, client, to); cn != "" {
		d.name = cn
	}
	return d, nil
}

func (s *Service) clientForChain(ctx context.Context, chainID int) (*ethclient.Client, error) {
	var urls []string
	if es, _ := s.st.ListRPCByChain(chainID); len(es) > 0 {
		for _, e := range es {
			urls = append(urls, e.URL)
		}
	} else if c, err := chains.Get(chainID); err == nil {
		urls = c.RPCs
	}
	for _, u := range urls {
		if cl, err := s.pool.Dial(ctx, u); err == nil {
			return cl, nil
		}
	}
	return nil, errors.New("no working RPC for that chain")
}

// --- wallet groups ---

func (s *Service) walletGroups() []string {
	ws, _ := s.st.ListWallets()
	seen := map[string]bool{}
	var gs []string
	for _, w := range ws {
		if w.Network != "evm" || seen[w.GroupName] {
			continue
		}
		seen[w.GroupName] = true
		gs = append(gs, w.GroupName)
	}
	sort.Strings(gs)
	return gs
}

func (s *Service) walletIDsForGroup(group string) []int64 {
	if group == "" {
		return nil // nil = all evm wallets
	}
	ws, _ := s.st.ListWallets()
	var ids []int64
	for _, w := range ws {
		if w.Network == "evm" && w.GroupName == group {
			ids = append(ids, w.ID)
		}
	}
	return ids
}

// nextGroup cycles through ["" (=all), <groups…>] for the Wallets toggle.
func (s *Service) nextGroup(cur string) string {
	gs := append([]string{""}, s.walletGroups()...)
	for i, g := range gs {
		if g == cur {
			return gs[(i+1)%len(gs)]
		}
	}
	return ""
}
