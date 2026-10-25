package httpproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/bits"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/fault"
)

type upstream struct {
	srv  *httptest.Server
	hits atomic.Int64
	body []byte
}

func startUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{body: bytes.Repeat([]byte("faultline-"), 10000)} // 100 KB
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		switch r.URL.Path {
		case "/big":
			w.Header().Set("Content-Length", strconv.Itoa(len(u.body)))
			_, _ = w.Write(u.body)
		case "/chunked":
			fl := w.(http.Flusher)
			for i := range 10 {
				_, _ = w.Write([]byte(strconv.Itoa(i)))
				fl.Flush()
			}
		case "/xff":
			_, _ = io.WriteString(w, r.Header.Get("X-Forwarded-For"))
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func startProxy(t *testing.T, up string, seed uint64) (*Proxy, *fault.Set) {
	t.Helper()
	set := fault.NewSet()
	p, err := Start(Config{Name: "edge", Listen: "127.0.0.1:0", Upstream: up, Faults: set, RNG: fault.NewRNG(seed)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, set
}

func addFault(t *testing.T, set *fault.Set, s fault.Spec) *fault.Active {
	t.Helper()
	if err := s.Validate(fault.KindHTTP); err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		s.ID = string(s.Type)
	}
	a := fault.NewActive(s, time.Now(), 0)
	if err := set.Add(a); err != nil {
		t.Fatal(err)
	}
	return a
}

var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

func TestPassthrough(t *testing.T) {
	u := startUpstream(t)
	p, _ := startProxy(t, u.srv.URL, 1)
	resp, body, err := get(t, "http://"+p.Addr()+"/xff", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "127.0.0.1" || resp.Header.Get(FaultHeader) != "" {
		t.Fatalf("status=%d body=%q fault=%q", resp.StatusCode, body, resp.Header.Get(FaultHeader))
	}
	if p.Stats().Requests != 1 || p.Stats().Faulted != 0 || p.Upstream() != u.srv.URL || p.Faults() == nil {
		t.Fatalf("stats %+v", p.Stats())
	}
}

func TestDelayAccuracy(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	measure := func() time.Duration {
		var ds []time.Duration
		for range 9 {
			start := time.Now()
			if _, _, err := get(t, "http://"+p.Addr()+"/", nil); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(start))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	base := measure()
	addFault(t, set, fault.Spec{Type: fault.Delay, Latency: fault.Duration(50 * time.Millisecond)})
	added := measure() - base
	if added < 48*time.Millisecond || added > 70*time.Millisecond {
		t.Fatalf("configured 50ms delay, measured %v", added)
	}
	if p.Stats().Delayed != 9 {
		t.Fatalf("delayed = %d", p.Stats().Delayed)
	}
}

func TestStatusInjection(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{ID: "boom", Type: fault.Status, Status: 503, Body: "nope",
		ResponseHeaders: map[string]string{"Retry-After": "1"}})
	resp, body, err := get(t, "http://"+p.Addr()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || string(body) != "nope" || resp.Header.Get("Retry-After") != "1" || resp.Header.Get(FaultHeader) != "boom" {
		t.Fatalf("status=%d body=%q headers=%v", resp.StatusCode, body, resp.Header)
	}
	if u.hits.Load() != 0 {
		t.Fatal("status injection must short-circuit the upstream")
	}
	set.Clear()
	addFault(t, set, fault.Spec{Type: fault.Status, Status: 500})
	_, body, _ = get(t, "http://"+p.Addr()+"/", nil)
	if string(body) != "faultline: injected 500 Internal Server Error\n" {
		t.Fatalf("default body %q", body)
	}
}

func TestStatusProbability(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 9)
	addFault(t, set, fault.Spec{Type: fault.Status, Status: 500, Probability: 0.3})
	fails := 0
	const n = 300
	for range n {
		resp, _, err := get(t, "http://"+p.Addr()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusInternalServerError {
			fails++
		}
	}
	if fails < n*20/100 || fails > n*40/100 {
		t.Fatalf("p=0.3 injected %d/%d", fails, n)
	}
}

func TestMatchScope(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{Type: fault.Status, Status: 418, Match: &fault.HTTPMatch{
		PathPrefix: "/api", Headers: map[string]string{"X-Faultline-Scope": "canary"}}})
	cases := []struct {
		path string
		hdr  map[string]string
		want int
	}{
		{"/api/x", map[string]string{"X-Faultline-Scope": "canary"}, 418},
		{"/api/x", nil, 200},
		{"/other", map[string]string{"X-Faultline-Scope": "canary"}, 200},
	}
	for _, c := range cases {
		resp, _, err := get(t, "http://"+p.Addr()+c.path, c.hdr)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != c.want {
			t.Fatalf("%s %v: got %d want %d", c.path, c.hdr, resp.StatusCode, c.want)
		}
	}
}

func TestAbort(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{Type: fault.Abort})
	_, _, err := get(t, "http://"+p.Addr()+"/", nil)
	if err == nil {
		t.Fatal("abort must surface as a transport error")
	}
	if p.Stats().Aborted != 1 || u.hits.Load() != 0 {
		t.Fatalf("stats %+v hits %d", p.Stats(), u.hits.Load())
	}
}

func TestCorruptFlipEveryByte(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{Type: fault.Corrupt, Rate: 1})
	_, body, err := get(t, "http://"+p.Addr()+"/big", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != len(u.body) {
		t.Fatalf("flip must preserve length: %d", len(body))
	}
	for i := range body {
		if bits.OnesCount8(body[i]^u.body[i]) != 1 {
			t.Fatalf("byte %d: want exactly one flipped bit", i)
		}
	}
	if p.Stats().Corrupted != 1 {
		t.Fatal("corrupted counter")
	}
}

func TestCorruptFlipRate(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 5)
	addFault(t, set, fault.Spec{Type: fault.Corrupt, Rate: 0.01})
	_, body, err := get(t, "http://"+p.Addr()+"/big", nil)
	if err != nil {
		t.Fatal(err)
	}
	diff := 0
	for i := range body {
		if body[i] != u.body[i] {
			diff++
		}
	}
	// 100,000 bytes at 1%: expect ~1000 (sd ~31).
	if diff < 850 || diff > 1150 {
		t.Fatalf("rate 0.01 corrupted %d of %d bytes", diff, len(body))
	}
}

func TestCorruptTruncate(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{Type: fault.Corrupt, Mode: fault.CorruptTruncate, AfterBytes: 100})
	// With Content-Length the client sees the stream die early.
	_, body, err := get(t, "http://"+p.Addr()+"/big", nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(body) != 100 {
		t.Fatalf("content-length truncate: %d bytes, err %v", len(body), err)
	}
	set.Clear()
	addFault(t, set, fault.Spec{Type: fault.Corrupt, Mode: fault.CorruptTruncate, AfterBytes: 4})
	// Chunked responses end cleanly but short.
	_, body, err = get(t, "http://"+p.Addr()+"/chunked", nil)
	if err != nil || string(body) != "0123" {
		t.Fatalf("chunked truncate: %q %v", body, err)
	}
}

func TestUpstreamDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()
	p, _ := startProxy(t, dead, 1)
	resp, _, err := get(t, "http://"+p.Addr()+"/", nil)
	if err != nil || resp.StatusCode != http.StatusBadGateway || p.Stats().UpstreamErrors != 1 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

func TestMaxAffected(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{Type: fault.Delay, Latency: fault.Duration(300 * time.Millisecond), MaxAffected: 1})
	var wg sync.WaitGroup
	durations := make([]time.Duration, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 1 {
				time.Sleep(50 * time.Millisecond) // let request 0 take the slot
			}
			start := time.Now()
			_, _, _ = get(t, "http://"+p.Addr()+"/", nil)
			durations[i] = time.Since(start)
		}()
	}
	wg.Wait()
	if durations[0] < 290*time.Millisecond || durations[1] > 200*time.Millisecond {
		t.Fatalf("durations %v: max_affected=1 not enforced", durations)
	}
}

func TestDelayCancelledByClient(t *testing.T) {
	u := startUpstream(t)
	p, set := startProxy(t, u.srv.URL, 1)
	addFault(t, set, fault.Spec{Type: fault.Delay, Latency: fault.Duration(5 * time.Second)})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.Addr()+"/", nil)
	start := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected client timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("client cancellation not honoured")
	}
	if u.hits.Load() != 0 {
		t.Fatal("cancelled request must not reach upstream")
	}
}

func TestStartErrors(t *testing.T) {
	for _, up := range []string{"ftp://x", "http://", "::bad"} {
		if _, err := Start(Config{Listen: "127.0.0.1:0", Upstream: up}); err == nil {
			t.Fatalf("upstream %q accepted", up)
		}
	}
	if _, err := Start(Config{Listen: "256.1.1.1:0", Upstream: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("bad listen accepted")
	}
}
