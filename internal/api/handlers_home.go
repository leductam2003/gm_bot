package api

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"zyperbot/internal/logger"
)

// GET /api/home — the Home dashboard payload: realized PNL (total + per-collection "top
// plays"), recent mint activity, minted/failed counts, and a spot ETH/USD price for the
// unit toggle. Realized PNL = Σ(sale proceeds − matched mint cost), aggregated in big.Int.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	minted, _ := s.st.CountMints("minted")
	failed, _ := s.st.CountMints("failed")
	recent, _ := s.st.RecentMints(50)
	sales, _ := s.st.AllSales()

	type agg struct {
		realized *big.Int
		contract string
		count    int
	}
	byColl := map[string]*agg{}
	total := new(big.Int)
	for _, sl := range sales {
		key := sl.Collection
		if key == "" {
			key = sl.Contract
		}
		a := byColl[key]
		if a == nil {
			a = &agg{realized: new(big.Int), contract: sl.Contract}
			byColl[key] = a
		}
		realized := new(big.Int).Sub(weiOrZero(sl.ProceedsWei), weiOrZero(sl.CostWei))
		a.realized.Add(a.realized, realized)
		a.count++
		total.Add(total, realized)
	}
	plays := make([]map[string]any, 0, len(byColl))
	for name, a := range byColl {
		plays = append(plays, map[string]any{
			"collection": name, "contract": a.contract,
			"realizedWei": a.realized.String(), "count": a.count,
		})
	}
	sort.Slice(plays, func(i, j int) bool { // biggest realized first
		ri, _ := new(big.Int).SetString(plays[i]["realizedWei"].(string), 10)
		rj, _ := new(big.Int).SetString(plays[j]["realizedWei"].(string), 10)
		return ri.Cmp(rj) > 0
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"pnlWei":   total.String(),
		"topPlays": plays,
		"activity": recent, // []store.Mint; null when empty — the client treats null as []
		"minted":   minted,
		"failed":   failed,
		"ethUsd":   s.ethUsd(r.Context()),
	})
}

// POST /api/home/reset — clear all recorded mints + sales (PNL and activity history).
func (s *Server) handleHomeReset(w http.ResponseWriter, r *http.Request) {
	if err := s.st.ResetActivity(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.API(logger.INFO, "home stats reset", nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func weiOrZero(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

// --- spot ETH/USD price (cached 60s) for the dashboard unit toggle ---

var ethUsdCache struct {
	mu    sync.Mutex
	price float64
	at    time.Time
}

// ethUsd returns a recent spot ETH/USD price from Coinbase's public endpoint. It sends no
// user data (just fetches a public price) and caches for 60s; returns the last value (or
// 0) on any error so the dashboard degrades gracefully.
func (s *Server) ethUsd(ctx context.Context) float64 {
	ethUsdCache.mu.Lock()
	defer ethUsdCache.mu.Unlock()
	if ethUsdCache.price > 0 && time.Since(ethUsdCache.at) < 60*time.Second {
		return ethUsdCache.price
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.coinbase.com/v2/prices/ETH-USD/spot", nil)
	if err != nil {
		return ethUsdCache.price
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return ethUsdCache.price
	}
	defer resp.Body.Close()
	var j struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&j) == nil {
		if v, e := strconv.ParseFloat(j.Data.Amount, 64); e == nil && v > 0 {
			ethUsdCache.price = v
			ethUsdCache.at = time.Now()
		}
	}
	return ethUsdCache.price
}
