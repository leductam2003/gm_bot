package evm

import (
	"context"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// NonceManager reserves nonces per address so concurrent sends for the same wallet
// don't collide — the port of zyper-mac/src/nonce.ts (reserve / invalidate).
type NonceManager struct {
	mu   sync.Mutex
	next map[common.Address]uint64
	has  map[common.Address]bool
}

func NewNonceManager() *NonceManager {
	return &NonceManager{next: map[common.Address]uint64{}, has: map[common.Address]bool{}}
}

// Reserve returns the next nonce for addr, fetching the pending nonce on first use
// and incrementing a local counter thereafter. Uses double-checked locking so the
// PendingNonceAt RPC does NOT run under the mutex — concurrent first-time reserves
// for different addresses fetch in parallel instead of serializing.
func (m *NonceManager) Reserve(ctx context.Context, c *ethclient.Client, addr common.Address) (uint64, error) {
	m.mu.Lock()
	if m.has[addr] {
		n := m.next[addr]
		m.next[addr] = n + 1
		m.mu.Unlock()
		return n, nil
	}
	m.mu.Unlock()

	// Fetch outside the lock.
	fetched, err := c.PendingNonceAt(ctx, addr)
	if err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.has[addr] {
		// We win the race: seed the counter from the fetched pending nonce.
		m.next[addr] = fetched
		m.has[addr] = true
	}
	// If another goroutine seeded it meanwhile, our `fetched` is discarded and we
	// take the next counter value — no collision either way.
	n := m.next[addr]
	m.next[addr] = n + 1
	return n, nil
}

// Invalidate drops the cached nonce for addr so the next Reserve refetches from the
// chain — used after a send failure to release the reserved nonce (mintEngine.ts).
func (m *NonceManager) Invalidate(addr common.Address) {
	m.mu.Lock()
	delete(m.next, addr)
	delete(m.has, addr)
	m.mu.Unlock()
}
