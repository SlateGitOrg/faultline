// Package tcpproxy implements faultline's L4 proxy: a TCP relay that applies
// latency, jitter, bandwidth caps, blackholing, FIN closes, slow closes and
// RST resets to the byte streams of individual connections.
package tcpproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/archit2501/faultline/internal/fault"
)

// Config configures a TCP proxy.
type Config struct {
	Name        string
	Listen      string
	Upstream    string
	Faults      *fault.Set
	RNG         *fault.RNG
	Logger      *slog.Logger
	DialTimeout time.Duration
}

// Stats are cumulative counters for one proxy.
type Stats struct {
	Accepted        int64 `json:"accepted"`
	Active          int64 `json:"active"`
	DialErrors      int64 `json:"dial_errors"`
	BytesUpstream   int64 `json:"bytes_upstream"`
	BytesDownstream int64 `json:"bytes_downstream"`
	BytesDropped    int64 `json:"bytes_dropped"`
	Resets          int64 `json:"resets"`
	Closes          int64 `json:"closes"`
}

type counters struct {
	accepted, active, dialErrors     atomic.Int64
	bytesUp, bytesDown, bytesDropped atomic.Int64
	resets, closes                   atomic.Int64
}

// Proxy relays connections from its listener to the upstream address.
type Proxy struct {
	cfg    Config
	ln     net.Listener
	log    *slog.Logger
	stats  counters
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[*session]struct{}
}

// Start binds the listener and begins accepting connections.
func Start(cfg Config) (*Proxy, error) {
	if cfg.Upstream == "" {
		return nil, errors.New("tcpproxy: upstream address is required")
	}
	if cfg.Faults == nil {
		cfg.Faults = fault.NewSet()
	}
	if cfg.RNG == nil {
		cfg.RNG = fault.NewRNG(0)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("tcpproxy %s: listen %s: %w", cfg.Name, cfg.Listen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{
		cfg:      cfg,
		ln:       ln,
		log:      cfg.Logger.With("proxy", cfg.Name, "kind", "tcp"),
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[*session]struct{}),
	}
	p.wg.Add(1)
	go p.acceptLoop()
	p.log.Info("tcp proxy listening", "listen", ln.Addr().String(), "upstream", cfg.Upstream)
	return p, nil
}

// Addr is the address the proxy is listening on.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Upstream is the address connections are relayed to.
func (p *Proxy) Upstream() string { return p.cfg.Upstream }

// Faults is the proxy's live fault registry.
func (p *Proxy) Faults() *fault.Set { return p.cfg.Faults }

// Stats returns a snapshot of the proxy's counters.
func (p *Proxy) Stats() Stats {
	s := &p.stats
	return Stats{
		Accepted:        s.accepted.Load(),
		Active:          s.active.Load(),
		DialErrors:      s.dialErrors.Load(),
		BytesUpstream:   s.bytesUp.Load(),
		BytesDownstream: s.bytesDown.Load(),
		BytesDropped:    s.bytesDropped.Load(),
		Resets:          s.resets.Load(),
		Closes:          s.closes.Load(),
	}
}

// Close stops accepting, tears down every open connection and waits for
// all goroutines to exit.
func (p *Proxy) Close() error {
	p.cancel()
	err := p.ln.Close()
	p.mu.Lock()
	for s := range p.sessions {
		s.shutdown()
	}
	p.mu.Unlock()
	p.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if p.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			p.log.Warn("accept failed", "err", err)
			continue
		}
		p.stats.accepted.Add(1)
		s := newSession(p, c.(*net.TCPConn))
		p.mu.Lock()
		p.sessions[s] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.stats.active.Add(1)
			s.run()
			p.stats.active.Add(-1)
			p.mu.Lock()
			delete(p.sessions, s)
			p.mu.Unlock()
		}()
	}
}
