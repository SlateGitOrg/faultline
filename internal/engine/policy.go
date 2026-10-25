package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/archit2501/faultline/internal/httpproxy"
)

// Resolver resolves host names for the upstream allowlist check.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type systemResolver struct{}

func (systemResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// ParseAllowlist parses a comma separated list of CIDRs or addresses.
func ParseAllowlist(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("invalid allowlist entry %q: %w", part, err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlist entry %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// resolveAllowed resolves host and returns its addresses if, and only if,
// every one of them is inside the allowlist. Requiring all addresses (not
// any) stops a name with one loopback and one public record from slipping
// through.
func (e *Engine) resolveAllowed(host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a.Unmap()}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := e.opts.Resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve upstream host %q: %w", ErrInvalid, host, err)
		}
		if len(res) == 0 {
			return nil, fmt.Errorf("%w: upstream host %q has no addresses", ErrInvalid, host)
		}
		for _, a := range res {
			addrs = append(addrs, a.Unmap())
		}
	}
	for _, a := range addrs {
		if !e.allowed(a) {
			return nil, fmt.Errorf("%w: upstream %s (%s) is outside the allowlist %v; widen it with --allow-upstream", ErrBlocked, host, a, e.opts.AllowUpstreams)
		}
	}
	return addrs, nil
}

func (e *Engine) allowed(a netip.Addr) bool {
	for _, p := range e.opts.AllowUpstreams {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// checkTCPUpstream enforces the allowlist and pins the upstream to the
// resolved address, so a later DNS change cannot redirect the proxy outside
// the allowlist.
func (e *Engine) checkTCPUpstream(upstream string) (string, error) {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil {
		return "", fmt.Errorf("%w: tcp upstream must be host:port: %w", ErrInvalid, err)
	}
	addrs, err := e.resolveAllowed(host)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(addrs[0].String(), port), nil
}

// checkHTTPUpstream enforces the allowlist for an HTTP upstream URL. The
// host name is kept (virtual hosting and TLS need it).
func (e *Engine) checkHTTPUpstream(upstream string) (string, error) {
	u, err := httpproxy.ParseUpstream(upstream)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveAllowed(u.Hostname()); err != nil {
		return "", err
	}
	return u.String(), nil
}
