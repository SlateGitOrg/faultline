// Package httpproxy implements faultline's L7 proxy: an HTTP reverse proxy
// that can delay requests, answer with injected status codes, abort client
// connections and corrupt response bodies, scoped per request.
package httpproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/archit2501/faultline/internal/fault"
)

// FaultHeader is set on every response a fault acted on, listing the fault
// IDs, so injected failures are always attributable when debugging.
const FaultHeader = "X-Faultline-Fault"

// Config configures an HTTP proxy.
type Config struct {
	Name      string
	Listen    string
	Upstream  string // base URL, e.g. http://127.0.0.1:8080
	Faults    *fault.Set
	RNG       *fault.RNG
	Logger    *slog.Logger
	Transport http.RoundTripper
}

// Stats are cumulative counters for one proxy.
type Stats struct {
	Requests       int64 `json:"requests"`
	Faulted        int64 `json:"faulted"`
	Delayed        int64 `json:"delayed"`
	StatusInjected int64 `json:"status_injected"`
	Aborted        int64 `json:"aborted"`
	Corrupted      int64 `json:"corrupted"`
	UpstreamErrors int64 `json:"upstream_errors"`
}

type counters struct {
	requests, faulted, delayed, status, aborted, corrupted, upstreamErrors atomic.Int64
}

// Proxy is a fault-injecting reverse proxy.
type Proxy struct {
	cfg      Config
	upstream *url.URL
	ln       net.Listener
	srv      *http.Server
	rp       *httputil.ReverseProxy
	log      *slog.Logger
	stats    counters
	done     chan struct{}
}

type ctxKey struct{}

// ParseUpstream validates an upstream base URL.
func ParseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("upstream URL %q must be http(s)://host[:port]", raw)
	}
	return u, nil
}

// Start binds the listener and begins serving.
func Start(cfg Config) (*Proxy, error) {
	u, err := ParseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
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
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("httpproxy %s: listen %s: %w", cfg.Name, cfg.Listen, err)
	}
	p := &Proxy{
		cfg:      cfg,
		upstream: u,
		ln:       ln,
		log:      cfg.Logger.With("proxy", cfg.Name, "kind", "http"),
		done:     make(chan struct{}),
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
		},
		Transport:      cfg.Transport,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.upstreamError,
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer close(p.done)
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.Error("http proxy stopped", "err", err)
		}
	}()
	p.log.Info("http proxy listening", "listen", ln.Addr().String(), "upstream", u.String())
	return p, nil
}

// Addr is the address the proxy is listening on.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Upstream is the base URL requests are forwarded to.
func (p *Proxy) Upstream() string { return p.upstream.String() }

// Faults is the proxy's live fault registry.
func (p *Proxy) Faults() *fault.Set { return p.cfg.Faults }

// Stats returns a snapshot of the proxy's counters.
func (p *Proxy) Stats() Stats {
	s := &p.stats
	return Stats{
		Requests:       s.requests.Load(),
		Faulted:        s.faulted.Load(),
		Delayed:        s.delayed.Load(),
		StatusInjected: s.status.Load(),
		Aborted:        s.aborted.Load(),
		Corrupted:      s.corrupted.Load(),
		UpstreamErrors: s.upstreamErrors.Load(),
	}
}

// Close stops the server and closes active connections.
func (p *Proxy) Close() error {
	err := p.srv.Close()
	<-p.done
	return err
}

// selectFaults returns the faults that act on r, reserving their
// blast-radius slots. The caller must release them.
func (p *Proxy) selectFaults(r *http.Request) []*fault.Active {
	var out []*fault.Active
	for _, a := range p.cfg.Faults.Live(time.Now()) {
		if a.MatchesRequest(r) && p.cfg.RNG.Chance(a.Probability) && a.Acquire() {
			a.MarkApplied()
			out = append(out, a)
		}
	}
	return out
}

// ServeHTTP applies faults in a fixed order: delay, abort, status, then
// forwarding with optional body corruption.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.stats.requests.Add(1)
	applied := p.selectFaults(r)
	defer func() {
		for _, a := range applied {
			a.Release()
		}
	}()
	if len(applied) == 0 {
		p.rp.ServeHTTP(w, r)
		return
	}
	p.stats.faulted.Add(1)
	ids := make([]string, len(applied))
	for i, a := range applied {
		ids[i] = a.ID
	}
	w.Header().Set(FaultHeader, strings.Join(ids, ","))

	var delay time.Duration
	var status, corrupt []*fault.Active
	abort := false
	for _, a := range applied {
		switch a.Type {
		case fault.Delay:
			delay += p.cfg.RNG.Jitter(a.Latency.D(), a.Jitter.D())
		case fault.Abort:
			abort = true
		case fault.Status:
			status = append(status, a)
		case fault.Corrupt:
			corrupt = append(corrupt, a)
		}
	}
	if delay > 0 {
		p.stats.delayed.Add(1)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	if abort {
		p.abort(w)
		return
	}
	if len(status) > 0 {
		p.injectStatus(w, status[0])
		return
	}
	if len(corrupt) > 0 {
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, corrupt))
	}
	p.rp.ServeHTTP(w, r)
}

func (p *Proxy) abort(w http.ResponseWriter) {
	p.stats.aborted.Add(1)
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		// HTTP/2 or a non-hijackable writer: the closest equivalent is a
		// handler panic, which net/http turns into a stream reset.
		panic(http.ErrAbortHandler)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
}

func (p *Proxy) injectStatus(w http.ResponseWriter, a *fault.Active) {
	p.stats.status.Add(1)
	for k, v := range a.ResponseHeaders {
		w.Header().Set(k, v)
	}
	body := a.Body
	if body == "" {
		body = fmt.Sprintf("faultline: injected %d %s\n", a.Status, http.StatusText(a.Status))
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(a.Status)
	_, _ = w.Write([]byte(body))
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	faults, _ := resp.Request.Context().Value(ctxKey{}).([]*fault.Active)
	if len(faults) == 0 {
		return nil
	}
	p.stats.corrupted.Add(1)
	body := resp.Body
	for _, a := range faults {
		body = newCorruptBody(body, a, p.cfg.RNG)
	}
	resp.Body = body
	return nil
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	p.stats.upstreamErrors.Add(1)
	p.log.Warn("upstream request failed", "path", r.URL.Path, "err", err)
	w.WriteHeader(http.StatusBadGateway)
}
