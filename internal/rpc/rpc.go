// Package rpc wraps go-ethereum's ethclient: a small cached pool of dialed
// clients, a latency probe (the "Test All" feature), and balance reads.
package rpc

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Pool caches one ethclient per URL so repeated balance/latency calls reuse the
// underlying HTTP connection.
type Pool struct {
	mu      sync.Mutex
	clients map[string]*ethclient.Client
}

func NewPool() *Pool { return &Pool{clients: map[string]*ethclient.Client{}} }

// Dial returns a cached client for url, dialing once and reusing thereafter.
func (p *Pool) Dial(ctx context.Context, url string) (*ethclient.Client, error) {
	return p.get(ctx, url)
}

func (p *Pool) get(ctx context.Context, url string) (*ethclient.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[url]; ok {
		return c, nil
	}
	c, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	p.clients[url] = c
	return c, nil
}

func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.Close()
	}
	p.clients = map[string]*ethclient.Client{}
}

// Probe is the result of a single latency test.
type Probe struct {
	URL       string `json:"url"`
	LatencyMs int64  `json:"latencyMs"` // -1 on failure
	OK        bool   `json:"ok"`
	Err       string `json:"err,omitempty"`
}

// TestLatency measures round-trip time of an eth_chainId-equivalent call.
func (p *Pool) TestLatency(ctx context.Context, url string) Probe {
	start := time.Now()
	c, err := p.get(ctx, url)
	if err != nil {
		return Probe{URL: url, LatencyMs: -1, OK: false, Err: err.Error()}
	}
	if _, err := c.ChainID(ctx); err != nil {
		return Probe{URL: url, LatencyMs: -1, OK: false, Err: err.Error()}
	}
	return Probe{URL: url, LatencyMs: time.Since(start).Milliseconds(), OK: true}
}

// TestAll probes every URL concurrently and returns results in input order.
func (p *Pool) TestAll(ctx context.Context, urls []string) []Probe {
	out := make([]Probe, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			out[i] = p.TestLatency(cctx, u)
		}(i, u)
	}
	wg.Wait()
	return out
}

// BaseFeeGwei returns the latest block's base fee in gwei (for the UI gas ticker).
func (p *Pool) BaseFeeGwei(ctx context.Context, url string) (float64, error) {
	c, err := p.get(ctx, url)
	if err != nil {
		return 0, err
	}
	head, err := c.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, err
	}
	if head.BaseFee == nil {
		return 0, nil
	}
	gwei := new(big.Float).Quo(new(big.Float).SetInt(head.BaseFee), big.NewFloat(1e9))
	f, _ := gwei.Float64()
	return f, nil
}

// Balance returns the native-token balance (wei) of addr via the given URL.
func (p *Pool) Balance(ctx context.Context, url, addr string) (*big.Int, error) {
	c, err := p.get(ctx, url)
	if err != nil {
		return nil, err
	}
	return c.BalanceAt(ctx, common.HexToAddress(addr), nil)
}

// BalanceResult pairs an address with its fetched balance.
type BalanceResult struct {
	Address    string `json:"address"`
	BalanceWei string `json:"balanceWei"`
	Err        string `json:"err,omitempty"`
}

// Balances fetches many addresses concurrently through a single URL (bounded).
func (p *Pool) Balances(ctx context.Context, url string, addrs []string) []BalanceResult {
	out := make([]BalanceResult, len(addrs))
	sem := make(chan struct{}, 8) // cap concurrency to avoid RPC rate limits
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func(i int, a string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			bal, err := p.Balance(cctx, url, a)
			if err != nil {
				out[i] = BalanceResult{Address: a, Err: err.Error()}
				return
			}
			out[i] = BalanceResult{Address: a, BalanceWei: bal.String()}
		}(i, a)
	}
	wg.Wait()
	return out
}
