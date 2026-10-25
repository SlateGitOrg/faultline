package engine

import (
	"sync"
	"time"
)

// Event is one entry in the audit log.
type Event struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Proxy  string    `json:"proxy,omitempty"`
	Fault  string    `json:"fault,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Audit is a bounded, in-memory ring buffer of control-plane events: every
// proxy and fault change, rejection, expiry and kill-switch action.
type Audit struct {
	mu   sync.Mutex
	buf  []Event
	next int
	full bool
	seq  int64
}

// NewAudit returns a log holding the most recent size events.
func NewAudit(size int) *Audit {
	return &Audit{buf: make([]Event, size)}
}

// Record appends an event, stamping its sequence number and time.
func (a *Audit) Record(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	ev.Seq = a.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	a.buf[a.next] = ev
	a.next = (a.next + 1) % len(a.buf)
	if a.next == 0 {
		a.full = true
	}
}

// Events returns events with Seq > since, oldest first.
func (a *Audit) Events(since int64) []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ordered []Event
	if a.full {
		ordered = append(ordered, a.buf[a.next:]...)
	}
	ordered = append(ordered, a.buf[:a.next]...)
	out := []Event{}
	for _, ev := range ordered {
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out
}
