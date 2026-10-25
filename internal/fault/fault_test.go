package fault

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	var s struct {
		D Duration `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"d":"150ms"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.D.D() != 150*time.Millisecond {
		t.Fatalf("got %v", s.D)
	}
	if err := json.Unmarshal([]byte(`{"d":2000000}`), &s); err != nil || s.D.D() != 2*time.Millisecond {
		t.Fatalf("numeric: %v %v", s.D, err)
	}
	if err := json.Unmarshal([]byte(`{"d":"soon"}`), &s); err == nil {
		t.Fatal("expected error for bad duration")
	}
	if err := json.Unmarshal([]byte(`{"d":true}`), &s); err == nil {
		t.Fatal("expected error for bool duration")
	}
	b, _ := json.Marshal(Duration(time.Second))
	if string(b) != `"1s"` {
		t.Fatalf("marshal: %s", b)
	}
	if Duration(time.Second).String() != "1s" {
		t.Fatal("String")
	}
}

func TestValidateDefaultsAndErrors(t *testing.T) {
	cases := []struct {
		name string
		kind Kind
		spec Spec
		err  string
	}{
		{"unknown type", KindTCP, Spec{Type: "nope"}, "unknown fault type"},
		{"wrong kind", KindHTTP, Spec{Type: Latency, Latency: Duration(time.Millisecond)}, "cannot be applied"},
		{"bad probability", KindTCP, Spec{Type: Reset, Probability: 1.5}, "probability"},
		{"negative ttl", KindTCP, Spec{Type: Reset, TTL: -1}, "ttl"},
		{"negative max affected", KindTCP, Spec{Type: Reset, MaxAffected: -1}, "max_affected"},
		{"negative latency", KindTCP, Spec{Type: Latency, Latency: -1}, "must not be negative"},
		{"huge latency", KindTCP, Spec{Type: Latency, Latency: Duration(time.Hour)}, "capped"},
		{"bad source", KindTCP, Spec{Type: Reset, Sources: []string{"nope"}}, "invalid source"},
		{"bad cidr", KindTCP, Spec{Type: Reset, Sources: []string{"10.0.0.0/99"}}, "invalid source CIDR"},
		{"http field on tcp", KindTCP, Spec{Type: Reset, Status: 500}, "does not accept HTTP"},
		{"bad direction", KindTCP, Spec{Type: Latency, Latency: 1, Direction: "sideways"}, "direction"},
		{"empty latency", KindTCP, Spec{Type: Latency}, "needs latency"},
		{"slow bandwidth", KindTCP, Spec{Type: Bandwidth, RateBPS: 1}, "rate_bps"},
		{"negative close", KindTCP, Spec{Type: Close, AfterBytes: -1}, "after_bytes"},
		{"slow close no delay", KindTCP, Spec{Type: SlowClose}, "needs a delay"},
		{"tcp field on http", KindHTTP, Spec{Type: Status, Status: 500, Direction: Upstream}, "does not accept TCP"},
		{"empty delay", KindHTTP, Spec{Type: Delay}, "needs latency"},
		{"bad status", KindHTTP, Spec{Type: Status, Status: 42}, "HTTP status"},
		{"bad mode", KindHTTP, Spec{Type: Corrupt, Mode: "melt"}, "corrupt mode"},
		{"bad rate", KindHTTP, Spec{Type: Corrupt, Rate: 2}, "corrupt rate"},
		{"bad truncate", KindHTTP, Spec{Type: Corrupt, Mode: CorruptTruncate, AfterBytes: -2}, "after_bytes"},
		{"bad path", KindHTTP, Spec{Type: Abort, Match: &HTTPMatch{PathPrefix: "x"}}, "path_prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate(tc.kind)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("want error containing %q, got %v", tc.err, err)
			}
		})
	}

	s := Spec{Type: Latency, Latency: Duration(time.Millisecond)}
	if err := s.Validate(KindTCP); err != nil {
		t.Fatal(err)
	}
	if s.Probability != 1 || s.Direction != Downstream {
		t.Fatalf("defaults not applied: %+v", s)
	}
	sc := Spec{Type: SlowClose, Delay: Duration(time.Millisecond)}
	if err := sc.Validate(KindTCP); err != nil || sc.Direction != Both {
		t.Fatalf("slow close default direction: %v %v", sc.Direction, err)
	}
	c := Spec{Type: Corrupt}
	if err := c.Validate(KindHTTP); err != nil || c.Mode != CorruptFlip || c.Rate != 0.01 {
		t.Fatalf("corrupt defaults: %+v %v", c, err)
	}
	m := Spec{Type: Status, Status: 503, Match: &HTTPMatch{Methods: []string{"get"}}}
	if err := m.Validate(KindHTTP); err != nil || m.Match.Methods[0] != http.MethodGet {
		t.Fatalf("method normalisation: %+v %v", m.Match, err)
	}
	for _, typ := range []Type{Reset, Blackhole, Close} {
		s := Spec{Type: typ}
		if err := s.Validate(KindTCP); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	for _, s := range []Spec{{Type: Abort}, {Type: Corrupt, Mode: CorruptTruncate, AfterBytes: 3}} {
		if err := s.Validate(KindHTTP); err != nil {
			t.Fatalf("%s: %v", s.Type, err)
		}
	}
}

func TestDirectionAndSources(t *testing.T) {
	s := Spec{Type: Latency, Latency: 1, Direction: Upstream, Sources: []string{"10.0.0.0/8", "127.0.0.1"}}
	if err := s.Validate(KindTCP); err != nil {
		t.Fatal(err)
	}
	if !s.AppliesTo(Upstream) || s.AppliesTo(Downstream) {
		t.Fatal("direction")
	}
	if !s.MatchesSource(&net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 9}) {
		t.Fatal("10.1.2.3 should match")
	}
	if !s.MatchesSource(&net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 9}) {
		t.Fatal("v4-mapped loopback should match")
	}
	if s.MatchesSource(&net.TCPAddr{IP: net.ParseIP("192.168.1.1"), Port: 9}) {
		t.Fatal("192.168.1.1 should not match")
	}
	if s.MatchesSource(nil) {
		t.Fatal("nil addr should not match a scoped fault")
	}
	if !s.MatchesSource(&net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1}) {
		t.Fatal("generic addr parsing")
	}
	if !s.MatchesRemote("127.0.0.1:5555") || s.MatchesRemote("8.8.8.8:53") || s.MatchesRemote("garbage") {
		t.Fatal("MatchesRemote")
	}
	open := Spec{Type: Reset}
	_ = open.Validate(KindTCP)
	if !open.MatchesSource(nil) || !open.MatchesRemote("x") {
		t.Fatal("unscoped fault must match everything")
	}
	both := Spec{Type: Latency, Latency: 1, Direction: Both}
	_ = both.Validate(KindTCP)
	if !both.AppliesTo(Upstream) || !both.AppliesTo(Downstream) {
		t.Fatal("both")
	}
}

func TestMatchesRequest(t *testing.T) {
	s := Spec{Type: Status, Status: 500, Match: &HTTPMatch{
		Methods: []string{http.MethodGet, "post"}, PathPrefix: "/api", Headers: map[string]string{"X-Canary": "1"},
	}, Sources: []string{"192.0.2.0/24"}}
	if err := s.Validate(KindHTTP); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	req.Header.Set("X-Canary", "1")
	if !s.MatchesRequest(req) {
		t.Fatal("should match")
	}
	req.Method = "POST"
	if !s.MatchesRequest(req) {
		t.Fatal("POST should match")
	}
	req.Method = "DELETE"
	if s.MatchesRequest(req) {
		t.Fatal("DELETE should not match")
	}
	req.Method = http.MethodGet
	req.Header.Del("X-Canary")
	if s.MatchesRequest(req) {
		t.Fatal("missing header should not match")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/other", nil)
	req2.Header.Set("X-Canary", "1")
	if s.MatchesRequest(req2) {
		t.Fatal("path should not match")
	}
	req3 := httptest.NewRequest(http.MethodGet, "/api", nil)
	req3.RemoteAddr = "10.0.0.1:1"
	req3.Header.Set("X-Canary", "1")
	if s.MatchesRequest(req3) {
		t.Fatal("source should not match")
	}
	any := Spec{Type: Abort}
	_ = any.Validate(KindHTTP)
	if !any.MatchesRequest(req3) {
		t.Fatal("unscoped")
	}
}

func TestKindOf(t *testing.T) {
	for _, typ := range []Type{Latency, Bandwidth, Reset, Close, SlowClose, Blackhole} {
		if k, ok := KindOf(typ); !ok || k != KindTCP {
			t.Fatalf("%s", typ)
		}
	}
	for _, typ := range []Type{Delay, Status, Abort, Corrupt} {
		if k, ok := KindOf(typ); !ok || k != KindHTTP {
			t.Fatalf("%s", typ)
		}
	}
	if _, ok := KindOf("x"); ok {
		t.Fatal("unknown")
	}
}

func TestRNG(t *testing.T) {
	a, b := NewRNG(7), NewRNG(7)
	for range 100 {
		if a.Float64() != b.Float64() {
			t.Fatal("same seed must give same sequence")
		}
	}
	g := NewRNG(1)
	if !g.Chance(1) || g.Chance(0) {
		t.Fatal("chance bounds")
	}
	hits := 0
	for range 10000 {
		if g.Chance(0.3) {
			hits++
		}
	}
	if hits < 2700 || hits > 3300 {
		t.Fatalf("chance(0.3) hit %d/10000", hits)
	}
	for range 1000 {
		j := g.Jitter(10*time.Millisecond, 5*time.Millisecond)
		if j < 5*time.Millisecond || j > 15*time.Millisecond {
			t.Fatalf("jitter out of range: %v", j)
		}
		if g.Jitter(time.Millisecond, 10*time.Millisecond) < 0 {
			t.Fatal("negative jitter result")
		}
		if n := g.IntN(8); n < 0 || n >= 8 {
			t.Fatal("IntN")
		}
	}
	if g.Jitter(time.Second, 0) != time.Second {
		t.Fatal("no jitter")
	}
	if NewRNG(0) == nil {
		t.Fatal("time seeded")
	}
}

func TestSetLifecycle(t *testing.T) {
	s := NewSet()
	now := time.Now()
	a := NewActive(Spec{ID: "a", Type: Reset}, now, 0)
	b := NewActive(Spec{ID: "b", Type: Reset}, now, time.Second)
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(NewActive(Spec{ID: "a"}, now, 0)); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if got := s.Live(now); len(got) != 2 {
		t.Fatalf("live = %d", len(got))
	}
	later := now.Add(2 * time.Second)
	if got := s.Live(later); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expired fault still live: %v", got)
	}
	if !b.Expired(later) || a.Expired(later.Add(time.Hour)) {
		t.Fatal("Expired")
	}
	if got := s.Sweep(later); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("sweep = %v", got)
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("get")
	}
	if _, ok := s.Get("zzz"); ok {
		t.Fatal("get missing")
	}
	if _, ok := s.Remove("a"); !ok {
		t.Fatal("remove")
	}
	if _, ok := s.Remove("a"); ok {
		t.Fatal("double remove")
	}
	_ = s.Add(NewActive(Spec{ID: "c"}, now, 0))
	if n := len(s.Clear()); n != 1 || len(s.All()) != 0 {
		t.Fatalf("clear removed %d", n)
	}
	info := b.Info()
	if info.ExpiresAt == nil || info.ID != "b" {
		t.Fatalf("info: %+v", info)
	}
	if a.Info().ExpiresAt != nil {
		t.Fatal("no-ttl info must have nil expiry")
	}
	// Live on a mixed slice keeps order of non-expired entries.
	s2 := NewSet()
	for i, ttl := range []time.Duration{0, time.Millisecond, 0, time.Millisecond, 0} {
		_ = s2.Add(NewActive(Spec{ID: string(rune('a' + i))}, now, ttl))
	}
	got := s2.Live(now.Add(time.Second))
	if len(got) != 3 || got[0].ID != "a" || got[1].ID != "c" || got[2].ID != "e" {
		t.Fatalf("live order: %v", got)
	}
}

func TestMaxAffectedAndDecider(t *testing.T) {
	a := NewActive(Spec{ID: "x", Type: Reset, Probability: 1, MaxAffected: 2}, time.Now(), 0)
	rng := NewRNG(1)
	d1, d2, d3 := NewDecider(rng, nil), NewDecider(rng, nil), NewDecider(rng, nil)
	if !d1.Applies(a) || !d2.Applies(a) {
		t.Fatal("first two sessions should be affected")
	}
	if d3.Applies(a) {
		t.Fatal("third session exceeds max_affected")
	}
	if !d1.Applies(a) || d3.Applies(a) {
		t.Fatal("decisions must be memoised")
	}
	if a.Info().Affected != 2 || a.Info().Applied != 2 {
		t.Fatalf("counters: %+v", a.Info())
	}
	d1.Release()
	d4 := NewDecider(rng, nil)
	if !d4.Applies(a) {
		t.Fatal("slot freed by release should be reusable")
	}
	scoped := NewDecider(rng, func(*Active) bool { return false })
	if scoped.Applies(NewActive(Spec{ID: "y", Probability: 1}, time.Now(), 0)) {
		t.Fatal("accept=false must veto")
	}
	never := NewActive(Spec{ID: "z", Probability: 0}, time.Now(), 0)
	if NewDecider(rng, nil).Applies(never) {
		t.Fatal("zero probability (not normalised) must not apply")
	}

	// Concurrent acquisition never exceeds the cap.
	c := NewActive(Spec{ID: "c", MaxAffected: 5}, time.Now(), 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Acquire() {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got != 5 {
		t.Fatalf("acquired %d slots, want 5", got)
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Spec{
		"latency 10ms±2ms downstream": {Type: Latency, Latency: Duration(10 * time.Millisecond), Jitter: Duration(2 * time.Millisecond), Direction: Downstream},
		"bandwidth 100 B/s":           {Type: Bandwidth, RateBPS: 100},
		"status 503 p=0.5 scoped":     {Type: Status, Status: 503, Probability: 0.5, Match: &HTTPMatch{}},
		"close after 10 B":            {Type: Close, AfterBytes: 10},
		"reset after 1s":              {Type: Reset, Delay: Duration(time.Second)},
		"corrupt flip":                {Type: Corrupt, Mode: "flip"},
		"abort":                       {Type: Abort},
	}
	for want, f := range cases {
		if got := f.Describe(); got != want {
			t.Fatalf("describe=%q want %q", got, want)
		}
	}
}
