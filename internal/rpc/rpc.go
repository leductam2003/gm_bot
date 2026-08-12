// Package rpc wraps go-ethereum's ethclient: a small cached pool of dialed
// clients, a latency probe (the "Test All" feature), and balance reads.
package rpc

import (
	"context"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

// sharedTransport is tuned for hundreds of concurrent in-flight RPC calls (mass
// mints fan out one goroutine per wallet). go-ethereum's default HTTP client uses
// http.DefaultTransport, which keeps only 2 idle connections per host — a burst of
// N calls pays a fresh TCP+TLS handshake almost every time — and, over HTTP/2,
// multiplexes everything onto ONE connection whose concurrency the server caps
// (often a few dozen streams), which serializes large batches. A big HTTP/1.1
// keep-alive pool gives predictable parallelism instead.
var sharedTransport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	// Sized for ~1000 tasks broadcasting to the same one or two RPC hosts at the
	// same start second: every connection warmed during pre-arm must survive in the
	// idle pool until T0, or the fire pays a fresh TCP+TLS handshake mid-race.
	MaxIdleConns:        8192,
	MaxIdleConnsPerHost: 4096,
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 10 * time.Second,
	// ForceAttemptHTTP2 left false: a custom transport without it speaks HTTP/1.1,
	// which is exactly what we want here (many parallel reused connections).
}

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
	var c *ethclient.Client
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// No http.Client.Timeout on purpose: it would cap EVERY call at one blanket
		// value, silently killing legitimately slow requests (a 200k-block getLogs
		// sales scan runs minutes under its own 15-minute context). Connect and TLS
		// are bounded by the transport; total call time is the caller's context.
		rc, err := gethrpc.DialOptions(ctx, url, gethrpc.WithHTTPClient(&http.Client{
			Transport: sharedTransport,
		}))
		if err != nil {
			return nil, err
		}
		c = ethclient.NewClient(rc)
	} else {
		// ws:// etc. — the custom HTTP client doesn't apply.
		cc, err := ethclient.DialContext(ctx, url)
		if err != nil {
			return nil, err
		}
		c = cc
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
