// Package engine owns the set of running proxies and every blast-radius
// guard rail around them: the upstream allowlist, fault TTLs, per-proxy and
// global caps, the kill switch and the audit log. The control API and the
// CLI are thin layers over it.
package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/archit2501/faultline/internal/fault"
	"github.com/archit2501/faultline/internal/httpproxy"
	"github.com/archit2501/faultline/internal/tcpproxy"
)

// Sentinel errors; the control API maps them to HTTP status codes.
var (
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrInvalid    = errors.New("invalid request")
	ErrBlocked    = errors.New("blocked by blast-radius policy")
	ErrKillSwitch = errors.New("kill switch engaged")
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Defaults for Options.
const (
	DefaultMaxTTL            = 30 * time.Minute
	DefaultMaxProxies        = 64
	DefaultMaxFaultsPerProxy = 32
	defaultSweepInterval     = 250 * time.Millisecond
)

// LoopbackOnly is the default upstream allowlist: faultline refuses to
// proxy anything that is not on this machine unless told otherwise.
var LoopbackOnly = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// Options configures an Engine. Zero values select safe defaults.
type Options struct {
	// AllowUpstreams lists the networks proxies may forward to.
	AllowUpstreams []netip.Prefix
	// MaxTTL bounds every fault's lifetime; faults without a TTL get MaxTTL.
	MaxTTL            time.Duration
	MaxProxies        int
	MaxFaultsPerProxy int
	// Seed makes fault sampling reproducible; 0 picks a random seed.
	Seed          uint64
	Logger        *slog.Logger
	SweepInterval time.Duration
	// Resolver resolves upstream host names for the allowlist check.
	Resolver Resolver
}

// ProxySpec declares a proxy.
type ProxySpec struct {
	Name     string     `json:"name"`
	Kind     fault.Kind `json:"kind"`
	Listen   string     `json:"listen"`
	Upstream string     `json:"upstream"`
}

// ProxyInfo describes a running proxy.
type ProxyInfo struct {
	Name     string           `json:"name"`
	Kind     fault.Kind       `json:"kind"`
	Listen   string           `json:"listen"`
	Upstream string           `json:"upstream"`
	Faults   []fault.Info     `json:"faults"`
	TCP      *tcpproxy.Stats  `json:"tcp_stats,omitempty"`
	HTTP     *httpproxy.Stats `json:"http_stats,omitempty"`
}

type entry struct {
	spec ProxySpec
	tcp  *tcpproxy.Proxy
	http *httpproxy.Proxy
	set  *fault.Set
}

func (e *entry) addr() string {
	if e.tcp != nil {
		return e.tcp.Addr()
	}
	return e.http.Addr()
}

func (e *entry) close() error {
	if e.tcp != nil {
		return e.tcp.Close()
	}
	return e.http.Close()
}

// KillState reports the kill switch.
type KillState struct {
	Engaged bool      `json:"engaged"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}

// Engine manages proxies and their faults.
type Engine struct {
	opts  Options
	log   *slog.Logger
	rng   *fault.RNG
	audit *Audit
	seq   atomic.Int64

	mu      sync.Mutex
	proxies map[string]*entry
	kill    KillState

	stop chan struct{}
	done chan struct{}
}

// New creates an engine and starts its TTL janitor.
func New(opts Options) *Engine {
	if len(opts.AllowUpstreams) == 0 {
		opts.AllowUpstreams = LoopbackOnly
	}
	if opts.MaxTTL <= 0 {
		opts.MaxTTL = DefaultMaxTTL
	}
	if opts.MaxProxies <= 0 {
		opts.MaxProxies = DefaultMaxProxies
	}
	if opts.MaxFaultsPerProxy <= 0 {
		opts.MaxFaultsPerProxy = DefaultMaxFaultsPerProxy
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = defaultSweepInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Resolver == nil {
		opts.Resolver = systemResolver{}
	}
	e := &Engine{
		opts:    opts,
		log:     opts.Logger,
		rng:     fault.NewRNG(opts.Seed),
		audit:   NewAudit(512),
		proxies: make(map[string]*entry),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go e.janitor()
	return e
}

// Options returns the effective options (after defaults).
func (e *Engine) Options() Options { return e.opts }

// Audit returns the engine's audit log.
func (e *Engine) Audit() *Audit { return e.audit }

func (e *Engine) janitor() {
	defer close(e.done)
	t := time.NewTicker(e.opts.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case now := <-t.C:
			e.sweep(now)
		}
	}
}

func (e *Engine) sweep(now time.Time) {
	e.mu.Lock()
	entries := make([]*entry, 0, len(e.proxies))
	for _, en := range e.proxies {
		entries = append(entries, en)
	}
	e.mu.Unlock()
	for _, en := range entries {
		for _, a := range en.set.Sweep(now) {
			e.log.Info("fault expired", "proxy", en.spec.Name, "fault", a.ID)
			e.audit.Record(Event{Action: "fault.expire", Proxy: en.spec.Name, Fault: a.ID, Detail: string(a.Type)})
		}
	}
}

// CreateProxy validates spec against policy and starts the proxy.
func (e *Engine) CreateProxy(spec ProxySpec) (ProxyInfo, error) {
	if !nameRE.MatchString(spec.Name) {
		return ProxyInfo{}, fmt.Errorf("%w: proxy name %q must match %s", ErrInvalid, spec.Name, nameRE)
	}
	if spec.Listen == "" {
		spec.Listen = "127.0.0.1:0"
	}
	var upstream string
	var err error
	switch spec.Kind {
	case fault.KindTCP:
		upstream, err = e.checkTCPUpstream(spec.Upstream)
	case fault.KindHTTP:
		upstream, err = e.checkHTTPUpstream(spec.Upstream)
	default:
		return ProxyInfo{}, fmt.Errorf("%w: proxy kind must be %q or %q, got %q", ErrInvalid, fault.KindTCP, fault.KindHTTP, spec.Kind)
	}
	if err != nil {
		e.audit.Record(Event{Action: "proxy.reject", Proxy: spec.Name, Detail: err.Error()})
		return ProxyInfo{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.proxies[spec.Name]; ok {
		return ProxyInfo{}, fmt.Errorf("%w: proxy %q", ErrExists, spec.Name)
	}
	if len(e.proxies) >= e.opts.MaxProxies {
		return ProxyInfo{}, fmt.Errorf("%w: at most %d proxies", ErrBlocked, e.opts.MaxProxies)
	}
	en := &entry{spec: spec, set: fault.NewSet()}
	switch spec.Kind {
	case fault.KindTCP:
		en.tcp, err = tcpproxy.Start(tcpproxy.Config{
			Name: spec.Name, Listen: spec.Listen, Upstream: upstream,
			Faults: en.set, RNG: e.rng, Logger: e.log,
		})
	case fault.KindHTTP:
		en.http, err = httpproxy.Start(httpproxy.Config{
			Name: spec.Name, Listen: spec.Listen, Upstream: upstream,
			Faults: en.set, RNG: e.rng, Logger: e.log,
		})
	}
	if err != nil {
		return ProxyInfo{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	e.proxies[spec.Name] = en
	e.audit.Record(Event{Action: "proxy.create", Proxy: spec.Name, Detail: fmt.Sprintf("%s %s -> %s", spec.Kind, en.addr(), upstream)})
	return e.info(en), nil
}

// DeleteProxy stops a proxy and drops its faults.
func (e *Engine) DeleteProxy(name string) error {
	e.mu.Lock()
	en, ok := e.proxies[name]
	delete(e.proxies, name)
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: proxy %q", ErrNotFound, name)
	}
	en.set.Clear()
	err := en.close()
	e.audit.Record(Event{Action: "proxy.delete", Proxy: name})
	return err
}

// Proxy describes one proxy.
func (e *Engine) Proxy(name string) (ProxyInfo, error) {
	en, err := e.get(name)
	if err != nil {
		return ProxyInfo{}, err
	}
	return e.info(en), nil
}

// Proxies describes every proxy, sorted by name.
func (e *Engine) Proxies() []ProxyInfo {
	e.mu.Lock()
	entries := make([]*entry, 0, len(e.proxies))
	for _, en := range e.proxies {
		entries = append(entries, en)
	}
	e.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].spec.Name < entries[j].spec.Name })
	out := make([]ProxyInfo, len(entries))
	for i, en := range entries {
		out[i] = e.info(en)
	}
	return out
}

func (e *Engine) get(name string) (*entry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.proxies[name]
	if !ok {
		return nil, fmt.Errorf("%w: proxy %q", ErrNotFound, name)
	}
	return en, nil
}

func (e *Engine) info(en *entry) ProxyInfo {
	now := time.Now()
	pi := ProxyInfo{Name: en.spec.Name, Kind: en.spec.Kind, Listen: en.addr(), Faults: []fault.Info{}}
	for _, a := range en.set.Live(now) {
		pi.Faults = append(pi.Faults, a.Info())
	}
	if en.tcp != nil {
		st := en.tcp.Stats()
		pi.TCP = &st
		pi.Upstream = en.tcp.Upstream()
	} else {
		st := en.http.Stats()
		pi.HTTP = &st
		pi.Upstream = en.http.Upstream()
	}
	return pi
}

// AddFault validates spec, applies TTL policy and installs it on a proxy.
func (e *Engine) AddFault(proxy string, spec fault.Spec) (fault.Info, error) {
	en, err := e.get(proxy)
	if err != nil {
		return fault.Info{}, err
	}
	reject := func(err error) (fault.Info, error) {
		e.audit.Record(Event{Action: "fault.reject", Proxy: proxy, Fault: spec.ID, Detail: err.Error()})
		return fault.Info{}, err
	}
	e.mu.Lock()
	kill := e.kill
	e.mu.Unlock()
	if kill.Engaged {
		return reject(fmt.Errorf("%w: %s", ErrKillSwitch, kill.Reason))
	}
	if err := spec.Validate(en.spec.Kind); err != nil {
		return reject(fmt.Errorf("%w: %w", ErrInvalid, err))
	}
	ttl := spec.TTL.D()
	if ttl == 0 {
		ttl = e.opts.MaxTTL
	}
	if ttl > e.opts.MaxTTL {
		return reject(fmt.Errorf("%w: ttl %s exceeds the maximum %s", ErrBlocked, ttl, e.opts.MaxTTL))
	}
	spec.TTL = fault.Duration(ttl)
	if spec.ID == "" {
		spec.ID = fmt.Sprintf("%s-%s-%d", proxy, spec.Type, e.seq.Add(1))
	} else if !nameRE.MatchString(spec.ID) {
		return reject(fmt.Errorf("%w: fault id %q must match %s", ErrInvalid, spec.ID, nameRE))
	}
	if n := len(en.set.Live(time.Now())); n >= e.opts.MaxFaultsPerProxy {
		return reject(fmt.Errorf("%w: proxy %q already has %d faults", ErrBlocked, proxy, n))
	}
	a := fault.NewActive(spec, time.Now(), ttl)
	if err := en.set.Add(a); err != nil {
		return reject(fmt.Errorf("%w: %w", ErrExists, err))
	}
	// Re-check after installing so a concurrent kill switch cannot leave a
	// fault behind.
	e.mu.Lock()
	killed := e.kill.Engaged
	e.mu.Unlock()
	if killed {
		en.set.Remove(a.ID)
		return reject(fmt.Errorf("%w: engaged while adding", ErrKillSwitch))
	}
	e.log.Info("fault added", "proxy", proxy, "fault", a.ID, "type", a.Type, "ttl", ttl)
	e.audit.Record(Event{Action: "fault.add", Proxy: proxy, Fault: a.ID, Detail: describe(a.Spec)})
	return a.Info(), nil
}

// RemoveFault uninstalls one fault.
func (e *Engine) RemoveFault(proxy, id string) error {
	en, err := e.get(proxy)
	if err != nil {
		return err
	}
	if _, ok := en.set.Remove(id); !ok {
		return fmt.Errorf("%w: fault %q on proxy %q", ErrNotFound, id, proxy)
	}
	e.log.Info("fault removed", "proxy", proxy, "fault", id)
	e.audit.Record(Event{Action: "fault.remove", Proxy: proxy, Fault: id})
	return nil
}

// ClearFaults uninstalls every fault on one proxy.
func (e *Engine) ClearFaults(proxy string) (int, error) {
	en, err := e.get(proxy)
	if err != nil {
		return 0, err
	}
	n := len(en.set.Clear())
	e.audit.Record(Event{Action: "fault.clear", Proxy: proxy, Detail: fmt.Sprintf("%d removed", n)})
	return n, nil
}

// ClearAll uninstalls every fault on every proxy.
func (e *Engine) ClearAll() int {
	e.mu.Lock()
	entries := make([]*entry, 0, len(e.proxies))
	for _, en := range e.proxies {
		entries = append(entries, en)
	}
	e.mu.Unlock()
	n := 0
	for _, en := range entries {
		n += len(en.set.Clear())
	}
	return n
}

// SetKillSwitch engages or releases the global kill switch. Engaging it
// removes every fault immediately and refuses new ones until released.
func (e *Engine) SetKillSwitch(engaged bool, reason string) KillState {
	e.mu.Lock()
	if engaged {
		e.kill = KillState{Engaged: true, Reason: reason, Since: time.Now()}
	} else {
		e.kill = KillState{}
	}
	st := e.kill
	e.mu.Unlock()
	if engaged {
		n := e.ClearAll()
		e.log.Warn("kill switch engaged", "reason", reason, "faults_removed", n)
		e.audit.Record(Event{Action: "killswitch.engage", Detail: fmt.Sprintf("%s (%d faults removed)", reason, n)})
	} else {
		e.log.Info("kill switch released")
		e.audit.Record(Event{Action: "killswitch.release"})
	}
	return st
}

// KillSwitch reports the kill switch state.
func (e *Engine) KillSwitch() KillState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kill
}

// Close stops every proxy and the janitor.
func (e *Engine) Close() error {
	close(e.stop)
	<-e.done
	e.mu.Lock()
	entries := e.proxies
	e.proxies = make(map[string]*entry)
	e.mu.Unlock()
	var errs []error
	for _, en := range entries {
		en.set.Clear()
		errs = append(errs, en.close())
	}
	return errors.Join(errs...)
}

func describe(s fault.Spec) string {
	switch s.Type {
	case fault.Latency, fault.Delay:
		return fmt.Sprintf("%s %s±%s p=%g", s.Type, s.Latency, s.Jitter, s.Probability)
	case fault.Bandwidth:
		return fmt.Sprintf("%s %dB/s p=%g", s.Type, s.RateBPS, s.Probability)
	case fault.Status:
		return fmt.Sprintf("%s %d p=%g", s.Type, s.Status, s.Probability)
	default:
		return fmt.Sprintf("%s p=%g", s.Type, s.Probability)
	}
}
