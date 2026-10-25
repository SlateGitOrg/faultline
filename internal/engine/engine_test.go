package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/fault"
)

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func newEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	if opts.Resolver == nil {
		opts.Resolver = fakeResolver{
			"svc.local":   {netip.MustParseAddr("127.0.0.5")},
			"mixed.local": {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("93.184.216.34")},
			"empty.local": {},
		}
	}
	e := New(opts)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func listener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestCreateProxyPolicy(t *testing.T) {
	e := newEngine(t, Options{})
	up := listener(t)
	cases := []struct {
		spec ProxySpec
		err  error
	}{
		{ProxySpec{Name: "ok-tcp", Kind: fault.KindTCP, Upstream: up}, nil},
		{ProxySpec{Name: "ok-http", Kind: fault.KindHTTP, Upstream: "http://" + up}, nil},
		{ProxySpec{Name: "by-name", Kind: fault.KindTCP, Upstream: "svc.local:80"}, nil},
		{ProxySpec{Name: "ok-tcp", Kind: fault.KindTCP, Upstream: up}, ErrExists},
		{ProxySpec{Name: "public", Kind: fault.KindTCP, Upstream: "93.184.216.34:80"}, ErrBlocked},
		{ProxySpec{Name: "public-http", Kind: fault.KindHTTP, Upstream: "http://10.1.1.1"}, ErrBlocked},
		{ProxySpec{Name: "mixed", Kind: fault.KindTCP, Upstream: "mixed.local:80"}, ErrBlocked},
		{ProxySpec{Name: "unresolvable", Kind: fault.KindTCP, Upstream: "nope.local:80"}, ErrInvalid},
		{ProxySpec{Name: "empty", Kind: fault.KindTCP, Upstream: "empty.local:80"}, ErrInvalid},
		{ProxySpec{Name: "noport", Kind: fault.KindTCP, Upstream: "127.0.0.1"}, ErrInvalid},
		{ProxySpec{Name: "badurl", Kind: fault.KindHTTP, Upstream: "127.0.0.1:80"}, ErrInvalid},
		{ProxySpec{Name: "bad name!", Kind: fault.KindTCP, Upstream: up}, ErrInvalid},
		{ProxySpec{Name: "udp", Kind: "udp", Upstream: up}, ErrInvalid},
		{ProxySpec{Name: "badlisten", Kind: fault.KindTCP, Listen: "256.0.0.1:1", Upstream: up}, ErrInvalid},
	}
	for _, c := range cases {
		_, err := e.CreateProxy(c.spec)
		if c.err == nil && err != nil {
			t.Fatalf("%s: unexpected %v", c.spec.Name, err)
		}
		if c.err != nil && !errors.Is(err, c.err) {
			t.Fatalf("%s: want %v, got %v", c.spec.Name, c.err, err)
		}
	}
	pi, err := e.Proxy("by-name")
	if err != nil || pi.Upstream != "127.0.0.5:80" {
		t.Fatalf("tcp upstream must be pinned to the resolved address: %+v %v", pi, err)
	}
	all := e.Proxies()
	if len(all) != 3 || all[0].Name != "by-name" || all[1].Name != "ok-http" || all[2].Name != "ok-tcp" {
		t.Fatalf("proxies not sorted: %+v", all)
	}
	if all[1].HTTP == nil || all[2].TCP == nil {
		t.Fatal("stats missing")
	}
	if _, err := e.Proxy("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing proxy")
	}
	if err := e.DeleteProxy("ok-tcp"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteProxy("ok-tcp"); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
	rejected := 0
	for _, ev := range e.Audit().Events(0) {
		if ev.Action == "proxy.reject" {
			rejected++
		}
	}
	if rejected < 6 {
		t.Fatalf("policy rejections not audited: %d", rejected)
	}
}

func TestWidenedAllowlist(t *testing.T) {
	allow, err := ParseAllowlist("10.0.0.0/8, 127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, Options{AllowUpstreams: allow})
	if _, err := e.checkTCPUpstream("10.2.3.4:80"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.checkTCPUpstream("127.0.0.2:80"); !errors.Is(err, ErrBlocked) {
		t.Fatal("127.0.0.2 is outside 127.0.0.1/32")
	}
	if _, err := ParseAllowlist("10.0.0.0/99"); err == nil {
		t.Fatal("bad prefix accepted")
	}
	if _, err := ParseAllowlist("nope"); err == nil {
		t.Fatal("bad addr accepted")
	}
	if p, _ := ParseAllowlist(" , "); len(p) != 0 {
		t.Fatal("empty entries")
	}
}

func TestProxyCap(t *testing.T) {
	e := newEngine(t, Options{MaxProxies: 1})
	up := listener(t)
	if _, err := e.CreateProxy(ProxySpec{Name: "a", Kind: fault.KindTCP, Upstream: up}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateProxy(ProxySpec{Name: "b", Kind: fault.KindTCP, Upstream: up}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
}

func TestFaultLifecycleAndTTL(t *testing.T) {
	e := newEngine(t, Options{MaxTTL: time.Minute, MaxFaultsPerProxy: 2, SweepInterval: 20 * time.Millisecond})
	up := listener(t)
	if _, err := e.CreateProxy(ProxySpec{Name: "p", Kind: fault.KindTCP, Upstream: up}); err != nil {
		t.Fatal(err)
	}
	in, err := e.AddFault("p", fault.Spec{Type: fault.Latency, Latency: fault.Duration(time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != "p-latency-1" || in.TTL.D() != time.Minute || in.ExpiresAt == nil {
		t.Fatalf("defaults: %+v", in)
	}
	if _, err := e.AddFault("p", fault.Spec{Type: fault.Reset, TTL: fault.Duration(time.Hour)}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("ttl above max must be blocked: %v", err)
	}
	if _, err := e.AddFault("p", fault.Spec{ID: "short", Type: fault.Reset, TTL: fault.Duration(60 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddFault("p", fault.Spec{Type: fault.Reset}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("per-proxy fault cap: %v", err)
	}
	if _, err := e.AddFault("p", fault.Spec{ID: "short", Type: fault.Reset}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := e.AddFault("p", fault.Spec{Type: fault.Status, Status: 500}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("http fault on tcp proxy: %v", err)
	}
	if _, err := e.AddFault("p", fault.Spec{ID: "bad id!", Type: fault.Reset}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := e.AddFault("missing", fault.Spec{Type: fault.Reset}); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing proxy")
	}
	// The short fault expires and the janitor sweeps and audits it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		pi, _ := e.Proxy("p")
		if len(pi.Faults) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fault did not expire")
		}
		time.Sleep(20 * time.Millisecond)
	}
	expired := false
	for time.Now().Before(deadline) && !expired {
		for _, ev := range e.Audit().Events(0) {
			if ev.Action == "fault.expire" && ev.Fault == "short" {
				expired = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !expired {
		t.Fatal("expiry not audited")
	}
	if err := e.RemoveFault("p", "p-latency-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveFault("p", "p-latency-1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("double remove")
	}
	if err := e.RemoveFault("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing proxy")
	}
	_, _ = e.AddFault("p", fault.Spec{Type: fault.Reset})
	if n, err := e.ClearFaults("p"); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if _, err := e.ClearFaults("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing proxy")
	}
}

func TestKillSwitch(t *testing.T) {
	e := newEngine(t, Options{})
	up := listener(t)
	_, _ = e.CreateProxy(ProxySpec{Name: "a", Kind: fault.KindTCP, Upstream: up})
	_, _ = e.CreateProxy(ProxySpec{Name: "b", Kind: fault.KindHTTP, Upstream: "http://" + up})
	_, _ = e.AddFault("a", fault.Spec{Type: fault.Reset})
	_, _ = e.AddFault("b", fault.Spec{Type: fault.Abort})
	st := e.SetKillSwitch(true, "pager fired")
	if !st.Engaged || st.Reason != "pager fired" || !e.KillSwitch().Engaged {
		t.Fatalf("state %+v", st)
	}
	for _, p := range e.Proxies() {
		if len(p.Faults) != 0 {
			t.Fatalf("kill switch left faults on %s", p.Name)
		}
	}
	if _, err := e.AddFault("a", fault.Spec{Type: fault.Reset}); !errors.Is(err, ErrKillSwitch) {
		t.Fatalf("faults must be refused while engaged: %v", err)
	}
	e.SetKillSwitch(false, "")
	if e.KillSwitch().Engaged {
		t.Fatal("release")
	}
	if _, err := e.AddFault("a", fault.Spec{Type: fault.Reset}); err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range e.Audit().Events(0) {
		actions = append(actions, ev.Action)
	}
	joined := strings.Join(actions, ",")
	for _, want := range []string{"killswitch.engage", "killswitch.release", "fault.reject"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("audit missing %s: %s", want, joined)
		}
	}
	if e.Options().MaxTTL != DefaultMaxTTL {
		t.Fatal("default options")
	}
}

func TestAuditRing(t *testing.T) {
	a := NewAudit(3)
	for i := range 5 {
		a.Record(Event{Action: string(rune('a' + i))})
	}
	ev := a.Events(0)
	if len(ev) != 3 || ev[0].Action != "c" || ev[2].Action != "e" || ev[2].Seq != 5 {
		t.Fatalf("ring: %+v", ev)
	}
	if got := a.Events(4); len(got) != 1 || got[0].Action != "e" {
		t.Fatalf("since: %+v", got)
	}
	if NewAudit(2).Events(0) == nil {
		t.Fatal("empty log must return an empty, non-nil slice")
	}
}

func TestDescribe(t *testing.T) {
	for _, s := range []fault.Spec{
		{Type: fault.Latency}, {Type: fault.Bandwidth, RateBPS: 10}, {Type: fault.Status, Status: 500}, {Type: fault.Reset},
	} {
		if d := describe(s); !strings.HasPrefix(d, string(s.Type)) {
			t.Fatalf("describe %q", d)
		}
	}
}

func TestSystemResolver(t *testing.T) {
	addrs, err := systemResolver{}.LookupNetIP(t.Context(), "ip", "localhost")
	if err != nil || len(addrs) == 0 {
		t.Skipf("localhost does not resolve here: %v", err)
	}
}
