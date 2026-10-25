package tcpproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/archit2501/faultline/internal/fault"
)

// chunkSize is the read buffer per direction. Small enough that latency is
// applied at fine granularity, large enough not to throttle throughput.
const chunkSize = 32 * 1024

// queueDepth bounds the chunks in flight per direction. A full queue blocks
// the reader, which is how back-pressure propagates to the sender, exactly
// like a real link's buffer.
const queueDepth = 64

// session is one proxied client connection and its upstream connection.
type session struct {
	p      *Proxy
	client *net.TCPConn
	up     *net.TCPConn
	start  time.Time
	dec    *fault.Decider
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	closed      bool
	resetArmed  map[*fault.Active]bool
	resetTimers []*time.Timer
}

func newSession(p *Proxy, client *net.TCPConn) *session {
	ctx, cancel := context.WithCancel(p.ctx)
	return &session{
		p:          p,
		client:     client,
		start:      time.Now(),
		dec:        fault.NewDecider(p.cfg.RNG, func(a *fault.Active) bool { return a.MatchesSource(client.RemoteAddr()) }),
		ctx:        ctx,
		cancel:     cancel,
		resetArmed: make(map[*fault.Active]bool),
	}
}

func (s *session) run() {
	defer s.dec.Release()
	defer s.shutdown()
	log := s.p.log.With("client", s.client.RemoteAddr().String())

	// Connection-level faults decided before the upstream is dialled.
	now := time.Now()
	for _, a := range s.p.cfg.Faults.Live(now) {
		switch {
		case a.Type == fault.Reset && a.Delay == 0 && s.dec.Applies(a):
			log.Debug("reset on accept", "fault", a.ID)
			s.reset()
			return
		case a.Type == fault.Close && a.AfterBytes == 0 && s.dec.Applies(a):
			log.Debug("close on accept", "fault", a.ID)
			s.p.stats.closes.Add(1)
			return
		}
	}

	d := net.Dialer{Timeout: s.p.cfg.DialTimeout}
	conn, err := d.DialContext(s.ctx, "tcp", s.p.cfg.Upstream)
	if err != nil {
		s.p.stats.dialErrors.Add(1)
		log.Warn("upstream dial failed", "upstream", s.p.cfg.Upstream, "err", err)
		return
	}
	s.mu.Lock()
	s.up = conn.(*net.TCPConn)
	closed := s.closed
	s.mu.Unlock()
	if closed {
		_ = conn.Close()
		return
	}
	s.armResets(time.Now())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.pipe(fault.Upstream, s.client, s.up) }()
	go func() { defer wg.Done(); s.pipe(fault.Downstream, s.up, s.client) }()
	wg.Wait()
	log.Debug("session finished", "duration", time.Since(s.start))
}

// armResets schedules an RST for every reset fault that applies to this
// session and has not been armed yet. It is called at connection start and
// on every chunk, so faults added mid-connection take effect on the next
// byte that flows.
func (s *session) armResets(now time.Time) {
	for _, a := range s.p.cfg.Faults.Live(now) {
		if a.Type != fault.Reset {
			continue
		}
		s.mu.Lock()
		armed := s.resetArmed[a]
		s.resetArmed[a] = true
		s.mu.Unlock()
		if armed || !s.dec.Applies(a) {
			continue
		}
		wait := a.Delay.D() - now.Sub(s.start)
		if wait <= 0 {
			s.reset()
			return
		}
		t := time.AfterFunc(wait, s.reset)
		s.mu.Lock()
		s.resetTimers = append(s.resetTimers, t)
		s.mu.Unlock()
	}
}

// reset aborts both connections with an RST (SO_LINGER=0).
func (s *session) reset() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	_ = s.client.SetLinger(0)
	if s.up != nil {
		_ = s.up.SetLinger(0)
	}
	s.mu.Unlock()
	s.p.stats.resets.Add(1)
	s.shutdown()
}

// shutdown closes both connections; safe to call repeatedly.
func (s *session) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	for _, t := range s.resetTimers {
		t.Stop()
	}
	_ = s.client.Close()
	if s.up != nil {
		_ = s.up.Close()
	}
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type chunk struct {
	data []byte
	at   time.Time
	err  error
}

// pipe relays one direction. A reader goroutine timestamps chunks as they
// arrive; the writer releases each chunk at arrival+latency, which keeps
// throughput intact under latency (chunks are delayed, not serialised).
func (s *session) pipe(dir fault.Direction, src, dst *net.TCPConn) {
	q := make(chan chunk, queueDepth)
	go s.read(src, q)
	w := writer{s: s, dir: dir, dst: dst}
	// Every path on which handle returns false either follows the reader's
	// final (error) chunk or cancels the session context, so the reader
	// never stays blocked on a full queue.
	for ck := range q {
		if !w.handle(ck) {
			return
		}
	}
}

func (s *session) read(src *net.TCPConn, q chan<- chunk) {
	defer close(q)
	buf := make([]byte, chunkSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case q <- chunk{data: data, at: time.Now()}:
			case <-s.ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case q <- chunk{err: err}:
			case <-s.ctx.Done():
			}
			return
		}
	}
}

// writer holds the per-direction state of the release side of a pipe.
type writer struct {
	s       *session
	dir     fault.Direction
	dst     *net.TCPConn
	lastDue time.Time
	sent    int64
	pace    pacer
}

// plan is the combined effect of all applicable faults on one chunk.
type plan struct {
	delay      time.Duration
	rate       int64
	closeAfter int64 // -1: no close fault
	blackhole  bool
	slowClose  time.Duration
}

func (w *writer) plan(now time.Time) plan {
	pl := plan{closeAfter: -1}
	rng := w.s.p.cfg.RNG
	for _, a := range w.s.p.cfg.Faults.Live(now) {
		if a.Type == fault.Reset || !a.AppliesTo(w.dir) || !w.s.dec.Applies(a) {
			continue
		}
		switch a.Type {
		case fault.Latency:
			pl.delay += rng.Jitter(a.Latency.D(), a.Jitter.D())
		case fault.Bandwidth:
			if pl.rate == 0 || a.RateBPS < pl.rate {
				pl.rate = a.RateBPS
			}
		case fault.Close:
			if pl.closeAfter < 0 || a.AfterBytes < pl.closeAfter {
				pl.closeAfter = a.AfterBytes
			}
		case fault.Blackhole:
			pl.blackhole = true
		case fault.SlowClose:
			pl.slowClose = max(pl.slowClose, a.Delay.D())
		}
	}
	return pl
}

// handle processes one chunk and reports whether the pipe should continue.
func (w *writer) handle(ck chunk) bool {
	s := w.s
	if ck.err != nil {
		if !errors.Is(ck.err, io.EOF) {
			// The peer reset or the session was torn down: propagate.
			s.shutdown()
			return false
		}
		if d := w.plan(time.Now()).slowClose; d > 0 {
			if !sleepUntil(s.ctx, time.Now().Add(d)) {
				return false
			}
		}
		_ = w.dst.CloseWrite()
		return false
	}
	now := time.Now()
	s.armResets(now)
	if s.isClosed() {
		return false
	}
	pl := w.plan(now)
	if pl.blackhole {
		s.p.stats.bytesDropped.Add(int64(len(ck.data)))
		return true
	}
	due := ck.at.Add(pl.delay)
	if due.Before(w.lastDue) {
		due = w.lastDue // jitter must never reorder bytes
	}
	w.lastDue = due
	if !sleepUntil(s.ctx, due) {
		return false
	}
	data := ck.data
	terminate := false
	if pl.closeAfter >= 0 {
		remaining := pl.closeAfter - w.sent
		if remaining <= 0 {
			data = nil
			terminate = true
		} else if int64(len(data)) >= remaining {
			data = data[:remaining]
			terminate = true
		}
	}
	if len(data) > 0 {
		var err error
		if pl.rate > 0 {
			err = w.pace.write(s.ctx, w.dst, data, pl.rate)
		} else {
			_, err = w.dst.Write(data)
		}
		if err != nil {
			s.shutdown()
			return false
		}
		w.sent += int64(len(data))
		if w.dir == fault.Upstream {
			s.p.stats.bytesUp.Add(int64(len(data)))
		} else {
			s.p.stats.bytesDown.Add(int64(len(data)))
		}
	}
	if terminate {
		s.p.stats.closes.Add(1)
		s.shutdown()
		return false
	}
	return true
}

// pacer spreads writes so that the long-run rate matches rate bytes/second.
type pacer struct {
	next time.Time
}

// slicesPerSecond controls pacing granularity: writes happen in slices of
// rate/slicesPerSecond bytes, i.e. roughly every 50ms.
const slicesPerSecond = 20

func (p *pacer) write(ctx context.Context, dst io.Writer, data []byte, rate int64) error {
	slice := max(int(rate/slicesPerSecond), 256)
	for len(data) > 0 {
		n := min(slice, len(data))
		now := time.Now()
		if p.next.Before(now) {
			p.next = now // no burst credit accumulates while idle
		}
		if !sleepUntil(ctx, p.next) {
			return ctx.Err()
		}
		if _, err := dst.Write(data[:n]); err != nil {
			return err
		}
		p.next = p.next.Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
		data = data[n:]
	}
	return nil
}

// sleepUntil waits until t or until ctx is done; it reports false if the
// context ended first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
