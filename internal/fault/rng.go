package fault

import (
	"math/rand/v2"
	"sync"
	"time"
)

// RNG is a goroutine-safe pseudo random source used for fault sampling
// (probability rolls, jitter, bit flips). It is deliberately math/rand based:
// fault injection needs reproducibility from a seed, not unpredictability.
type RNG struct {
	mu sync.Mutex
	r  *rand.Rand
}

// NewRNG returns a generator seeded with seed. A seed of 0 picks a
// time-based seed.
func NewRNG(seed uint64) *RNG {
	if seed == 0 {
		seed = uint64(time.Now().UnixNano()) //nolint:gosec // sign is irrelevant for a seed
	}
	return &RNG{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // non-crypto sampling by design
}

// Float64 returns a value in [0,1).
func (g *RNG) Float64() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.r.Float64()
}

// Chance reports true with probability p. p >= 1 is always true.
func (g *RNG) Chance(p float64) bool {
	if p >= 1 {
		return true
	}
	if p <= 0 {
		return false
	}
	return g.Float64() < p
}

// IntN returns a value in [0,n). n must be > 0.
func (g *RNG) IntN(n int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.r.IntN(n)
}

// Jitter returns base shifted uniformly within [-jitter, +jitter], clamped to
// be non-negative.
func (g *RNG) Jitter(base, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return base
	}
	g.mu.Lock()
	off := time.Duration(g.r.Int64N(int64(2*jitter)+1)) - jitter
	g.mu.Unlock()
	d := base + off
	if d < 0 {
		return 0
	}
	return d
}
