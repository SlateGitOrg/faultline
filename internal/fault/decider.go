package fault

import (
	"sync"
)

// Decider memoises per-session fault decisions. A session is one TCP
// connection: probability, source scope and the MaxAffected cap are evaluated
// once per (session, fault) so that a probabilistic fault affects whole
// connections instead of a random subset of their chunks.
type Decider struct {
	rng      *RNG
	accept   func(*Active) bool
	mu       sync.Mutex
	decided  map[*Active]bool
	acquired []*Active
}

// NewDecider builds a decider. accept performs scope checks that do not
// depend on chance (for example the client source address); it may be nil.
func NewDecider(rng *RNG, accept func(*Active) bool) *Decider {
	return &Decider{rng: rng, accept: accept, decided: make(map[*Active]bool)}
}

// Applies reports whether fault a applies to this session, deciding (and
// reserving a blast-radius slot) the first time a is seen.
func (d *Decider) Applies(a *Active) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.decided[a]; ok {
		return v
	}
	v := (d.accept == nil || d.accept(a)) && d.rng.Chance(a.Probability) && a.Acquire()
	if v {
		d.acquired = append(d.acquired, a)
		a.MarkApplied()
	}
	d.decided[a] = v
	return v
}

// Release frees every blast-radius slot this session reserved. It must be
// called once when the session ends.
func (d *Decider) Release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.acquired {
		a.Release()
	}
	d.acquired = nil
}
