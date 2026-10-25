// Package fault defines fault specifications, their validation and the
// thread-safe registry of active faults that proxies consult on every
// connection, chunk and request.
package fault

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Kind is the protocol layer a proxy (and therefore a fault) operates at.
type Kind string

// Proxy kinds.
const (
	KindTCP  Kind = "tcp"
	KindHTTP Kind = "http"
)

// Type names a fault behaviour.
type Type string

// L4 (TCP) fault types.
const (
	// Latency delays every chunk flowing in Direction by Latency ± Jitter.
	Latency Type = "latency"
	// Bandwidth caps throughput in Direction to RateBPS bytes per second.
	Bandwidth Type = "bandwidth"
	// Reset aborts the connection with a TCP RST after Delay.
	Reset Type = "reset"
	// Close closes the connection (FIN) once AfterBytes have flowed in Direction.
	Close Type = "close"
	// SlowClose holds a half-close in Direction for Delay before propagating it.
	SlowClose Type = "slow_close"
	// Blackhole silently discards data in Direction while keeping the connection open.
	Blackhole Type = "blackhole"
)

// L7 (HTTP) fault types.
const (
	// Delay holds the request for Latency ± Jitter before forwarding it.
	Delay Type = "delay"
	// Status answers with Status (and Body) without contacting the upstream.
	Status Type = "status"
	// Abort drops the client connection without any HTTP response.
	Abort Type = "abort"
	// Corrupt mangles the upstream response body according to Mode.
	Corrupt Type = "corrupt"
)

// Direction selects which half of a TCP connection a fault applies to.
type Direction string

// Directions. Upstream is client→server, downstream is server→client.
const (
	Upstream   Direction = "upstream"
	Downstream Direction = "downstream"
	Both       Direction = "both"
)

// Corruption modes for the Corrupt fault.
const (
	CorruptFlip     = "flip"
	CorruptTruncate = "truncate"
)

// Server-side guard rails that bound what any single fault may do.
const (
	MaxLatency = 5 * time.Minute
	MinRateBPS = 64
)

// HTTPMatch scopes an HTTP fault to a subset of requests. All set fields
// must match (logical AND); within Methods any entry may match.
type HTTPMatch struct {
	Methods    []string          `json:"methods,omitempty"`
	PathPrefix string            `json:"path_prefix,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// Spec is the declarative description of one fault. A single flat struct is
// used for every type so the same JSON/YAML shape works for the control API
// and for experiment files; Validate rejects fields that do not belong to
// the declared type's kind.
type Spec struct {
	ID          string    `json:"id,omitempty"`
	Type        Type      `json:"type"`
	Direction   Direction `json:"direction,omitempty"`
	Probability float64   `json:"probability,omitempty"`
	TTL         Duration  `json:"ttl,omitempty"`
	MaxAffected int       `json:"max_affected,omitempty"`
	Sources     []string  `json:"sources,omitempty"`

	Latency    Duration `json:"latency,omitempty"`
	Jitter     Duration `json:"jitter,omitempty"`
	RateBPS    int64    `json:"rate_bps,omitempty"`
	AfterBytes int64    `json:"after_bytes,omitempty"`
	Delay      Duration `json:"delay,omitempty"`

	Match           *HTTPMatch        `json:"match,omitempty"`
	Status          int               `json:"status,omitempty"`
	Body            string            `json:"body,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	Mode            string            `json:"mode,omitempty"`
	Rate            float64           `json:"rate,omitempty"`

	sources []netip.Prefix
}

// KindOf reports which proxy kind a fault type belongs to.
func KindOf(t Type) (Kind, bool) {
	switch t {
	case Latency, Bandwidth, Reset, Close, SlowClose, Blackhole:
		return KindTCP, true
	case Delay, Status, Abort, Corrupt:
		return KindHTTP, true
	}
	return "", false
}

// Validate checks the spec against a proxy kind and fills in defaults
// (probability 1, default directions, parsed source prefixes). It must be
// called before a spec is activated.
func (s *Spec) Validate(kind Kind) error {
	k, ok := KindOf(s.Type)
	if !ok {
		return fmt.Errorf("unknown fault type %q", s.Type)
	}
	if k != kind {
		return fmt.Errorf("fault type %q is a %s fault and cannot be applied to a %s proxy", s.Type, k, kind)
	}
	if s.Probability < 0 || s.Probability > 1 {
		return fmt.Errorf("probability must be within [0,1], got %v", s.Probability)
	}
	if s.Probability == 0 {
		s.Probability = 1
	}
	if s.TTL < 0 {
		return errors.New("ttl must not be negative")
	}
	if s.MaxAffected < 0 {
		return errors.New("max_affected must not be negative")
	}
	if s.Latency < 0 || s.Jitter < 0 || s.Delay < 0 {
		return errors.New("latency, jitter and delay must not be negative")
	}
	if s.Latency.D() > MaxLatency || s.Jitter.D() > MaxLatency || s.Delay.D() > MaxLatency {
		return fmt.Errorf("latency, jitter and delay are capped at %s", MaxLatency)
	}
	s.sources = nil
	for _, src := range s.Sources {
		p, err := parsePrefix(src)
		if err != nil {
			return err
		}
		s.sources = append(s.sources, p)
	}
	if kind == KindTCP {
		return s.validateTCP()
	}
	return s.validateHTTP()
}

func parsePrefix(src string) (netip.Prefix, error) {
	if strings.Contains(src, "/") {
		p, err := netip.ParsePrefix(src)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("invalid source CIDR %q: %w", src, err)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(src)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid source address %q: %w", src, err)
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

func (s *Spec) validateTCP() error {
	if s.Match != nil || s.Status != 0 || s.Body != "" || len(s.ResponseHeaders) > 0 || s.Mode != "" || s.Rate != 0 {
		return fmt.Errorf("%s fault does not accept HTTP fields (match, status, body, response_headers, mode, rate)", s.Type)
	}
	switch s.Direction {
	case "":
		if s.Type == SlowClose {
			s.Direction = Both
		} else {
			s.Direction = Downstream
		}
	case Upstream, Downstream, Both:
	default:
		return fmt.Errorf("direction must be upstream, downstream or both, got %q", s.Direction)
	}
	switch s.Type {
	case Latency:
		if s.Latency == 0 && s.Jitter == 0 {
			return errors.New("latency fault needs latency and/or jitter")
		}
	case Bandwidth:
		if s.RateBPS < MinRateBPS {
			return fmt.Errorf("bandwidth fault needs rate_bps >= %d", MinRateBPS)
		}
	case Close:
		if s.AfterBytes < 0 {
			return errors.New("after_bytes must not be negative")
		}
	case SlowClose:
		if s.Delay == 0 {
			return errors.New("slow_close fault needs a delay")
		}
	case Reset, Blackhole:
	}
	return nil
}

func (s *Spec) validateHTTP() error {
	if s.Direction != "" || s.RateBPS != 0 || (s.AfterBytes != 0 && s.Type != Corrupt) {
		return fmt.Errorf("%s fault does not accept TCP fields (direction, rate_bps, after_bytes)", s.Type)
	}
	switch s.Type {
	case Delay:
		if s.Latency == 0 && s.Jitter == 0 {
			return errors.New("delay fault needs latency and/or jitter")
		}
	case Status:
		if s.Status < 100 || s.Status > 599 {
			return fmt.Errorf("status fault needs an HTTP status in [100,599], got %d", s.Status)
		}
	case Corrupt:
		switch s.Mode {
		case "", CorruptFlip:
			s.Mode = CorruptFlip
			if s.Rate == 0 {
				s.Rate = 0.01
			}
			if s.Rate < 0 || s.Rate > 1 {
				return fmt.Errorf("corrupt rate must be within (0,1], got %v", s.Rate)
			}
		case CorruptTruncate:
			if s.AfterBytes < 0 {
				return errors.New("after_bytes must not be negative")
			}
		default:
			return fmt.Errorf("corrupt mode must be %q or %q, got %q", CorruptFlip, CorruptTruncate, s.Mode)
		}
	case Abort:
	}
	if s.Match != nil {
		for i, m := range s.Match.Methods {
			s.Match.Methods[i] = strings.ToUpper(m)
		}
		if s.Match.PathPrefix != "" && !strings.HasPrefix(s.Match.PathPrefix, "/") {
			return fmt.Errorf("match.path_prefix must start with '/', got %q", s.Match.PathPrefix)
		}
	}
	return nil
}

// AppliesTo reports whether the fault acts on data flowing in d.
func (s *Spec) AppliesTo(d Direction) bool {
	return s.Direction == Both || s.Direction == d
}

// MatchesSource reports whether a client address is inside the fault's
// source scope. An empty scope matches every client.
func (s *Spec) MatchesSource(addr net.Addr) bool {
	if len(s.sources) == 0 {
		return true
	}
	ip, ok := addrIP(addr)
	if !ok {
		return false
	}
	for _, p := range s.sources {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// MatchesRemote is MatchesSource for an HTTP request's RemoteAddr string.
func (s *Spec) MatchesRemote(remote string) bool {
	if len(s.sources) == 0 {
		return true
	}
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	for _, p := range s.sources {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func addrIP(a net.Addr) (netip.Addr, bool) {
	switch v := a.(type) {
	case *net.TCPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		return ip.Unmap(), ok
	case nil:
		return netip.Addr{}, false
	default:
		ap, err := netip.ParseAddrPort(a.String())
		if err != nil {
			return netip.Addr{}, false
		}
		return ap.Addr().Unmap(), true
	}
}

// MatchesRequest reports whether an HTTP request is in the fault's scope
// (method, path prefix, headers and client source).
func (s *Spec) MatchesRequest(r *http.Request) bool {
	if !s.MatchesRemote(r.RemoteAddr) {
		return false
	}
	m := s.Match
	if m == nil {
		return true
	}
	if len(m.Methods) > 0 {
		found := false
		for _, meth := range m.Methods {
			if meth == r.Method {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if m.PathPrefix != "" && !strings.HasPrefix(r.URL.Path, m.PathPrefix) {
		return false
	}
	for k, v := range m.Headers {
		if r.Header.Get(k) != v {
			return false
		}
	}
	return true
}

// Describe renders the spec compactly for logs, audit entries and reports,
// e.g. "latency 100ms±10ms downstream p=0.5".
func (f *Spec) Describe() string {
	s := string(f.Type)
	switch f.Type {
	case Latency, Delay:
		s += " " + f.Latency.String()
		if f.Jitter > 0 {
			s += "±" + f.Jitter.String()
		}
	case Bandwidth:
		s += fmt.Sprintf(" %d B/s", f.RateBPS)
	case Status:
		s += fmt.Sprintf(" %d", f.Status)
	case Close:
		s += fmt.Sprintf(" after %d B", f.AfterBytes)
	case SlowClose, Reset:
		if f.Delay > 0 {
			s += " after " + f.Delay.String()
		}
	case Corrupt:
		s += " " + f.Mode
		switch {
		case f.Mode == CorruptTruncate:
			s += fmt.Sprintf(" after %d B", f.AfterBytes)
		case f.Rate > 0:
			s += fmt.Sprintf(" rate=%g", f.Rate)
		}
	}
	if f.Direction != "" && f.Type != Reset {
		s += " " + string(f.Direction)
	}
	if f.Probability > 0 && f.Probability < 1 {
		s += fmt.Sprintf(" p=%g", f.Probability)
	}
	if f.Match != nil {
		s += " scoped"
	}
	return s
}
