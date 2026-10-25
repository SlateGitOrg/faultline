package fault

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Active is a validated fault that is installed on a proxy. Proxies read it
// concurrently; its counters are atomic.
type Active struct {
	Spec
	CreatedAt time.Time
	// ExpiresAt is when the fault stops applying. The zero value means never.
	ExpiresAt time.Time

	applied  atomic.Int64
	affected atomic.Int64
}

// NewActive wraps a validated spec. ttl <= 0 means the fault never expires.
func NewActive(spec Spec, now time.Time, ttl time.Duration) *Active {
	a := &Active{Spec: spec, CreatedAt: now}
	if ttl > 0 {
		a.ExpiresAt = now.Add(ttl)
	}
	return a
}

// Expired reports whether the fault's TTL has elapsed at now.
func (a *Active) Expired(now time.Time) bool {
	return !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt)
}

// Acquire reserves one slot of the fault's concurrent blast radius
// (MaxAffected). It returns false when the cap is reached; every successful
// Acquire must be paired with Release.
func (a *Active) Acquire() bool {
	if a.MaxAffected <= 0 {
		a.affected.Add(1)
		return true
	}
	for {
		cur := a.affected.Load()
		if cur >= int64(a.MaxAffected) {
			return false
		}
		if a.affected.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release frees a slot reserved by Acquire.
func (a *Active) Release() { a.affected.Add(-1) }

// MarkApplied counts one application of the fault (a connection, chunk
// decision or request that the fault acted on).
func (a *Active) MarkApplied() { a.applied.Add(1) }

// Info is a JSON-friendly snapshot of an active fault.
type Info struct {
	Spec
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Applied   int64      `json:"applied"`
	Affected  int64      `json:"affected_now"`
}

// Info returns a snapshot suitable for the control API.
func (a *Active) Info() Info {
	in := Info{Spec: a.Spec, CreatedAt: a.CreatedAt, Applied: a.applied.Load(), Affected: a.affected.Load()}
	if !a.ExpiresAt.IsZero() {
		t := a.ExpiresAt
		in.ExpiresAt = &t
	}
	return in
}

// Set is the registry of faults installed on one proxy. Reads are lock-free
// (an atomically swapped immutable slice) because proxies consult it on
// every chunk; writes are serialised by a mutex and copy the slice.
type Set struct {
	mu  sync.Mutex
	cur atomic.Pointer[[]*Active]
}

// NewSet returns an empty registry.
func NewSet() *Set {
	s := &Set{}
	empty := []*Active{}
	s.cur.Store(&empty)
	return s
}

// All returns every installed fault, including expired ones not yet swept.
// The returned slice must not be modified.
func (s *Set) All() []*Active { return *s.cur.Load() }

// Live returns the faults that have not expired at now. It does not allocate
// when nothing has expired.
func (s *Set) Live(now time.Time) []*Active {
	all := s.All()
	for i, a := range all {
		if a.Expired(now) {
			out := make([]*Active, 0, len(all)-1)
			out = append(out, all[:i]...)
			for _, b := range all[i+1:] {
				if !b.Expired(now) {
					out = append(out, b)
				}
			}
			return out
		}
	}
	return all
}

// Add installs a fault. IDs must be unique within the set.
func (s *Set) Add(a *Active) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := *s.cur.Load()
	for _, b := range old {
		if b.ID == a.ID {
			return fmt.Errorf("fault %q already exists", a.ID)
		}
	}
	next := make([]*Active, len(old), len(old)+1)
	copy(next, old)
	next = append(next, a)
	s.cur.Store(&next)
	return nil
}

// Get returns the fault with the given ID.
func (s *Set) Get(id string) (*Active, bool) {
	for _, a := range s.All() {
		if a.ID == id {
			return a, true
		}
	}
	return nil, false
}

// Remove uninstalls a fault by ID.
func (s *Set) Remove(id string) (*Active, bool) {
	var removed *Active
	s.filter(func(a *Active) bool {
		if a.ID == id {
			removed = a
			return false
		}
		return true
	})
	return removed, removed != nil
}

// Clear uninstalls every fault and returns what was removed.
func (s *Set) Clear() []*Active {
	return s.filter(func(*Active) bool { return false })
}

// Sweep removes faults that have expired at now and returns them.
func (s *Set) Sweep(now time.Time) []*Active {
	return s.filter(func(a *Active) bool { return !a.Expired(now) })
}

// filter keeps faults for which keep returns true and returns the rest.
func (s *Set) filter(keep func(*Active) bool) []*Active {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := *s.cur.Load()
	next := make([]*Active, 0, len(old))
	var dropped []*Active
	for _, a := range old {
		if keep(a) {
			next = append(next, a)
		} else {
			dropped = append(dropped, a)
		}
	}
	if len(dropped) > 0 {
		s.cur.Store(&next)
	}
	return dropped
}
