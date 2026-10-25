package tcpproxy

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SlateGitOrg/faultline/internal/fault"
)

// startEcho runs a TCP echo server that half-closes after the client does.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_, _ = io.Copy(c, c)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return ln.Addr().String()
}

func startProxy(t *testing.T, upstream string, seed uint64) (*Proxy, *fault.Set) {
	t.Helper()
	set := fault.NewSet()
	p, err := Start(Config{Name: "t", Listen: "127.0.0.1:0", Upstream: upstream, Faults: set, RNG: fault.NewRNG(seed)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, set
}

func addFault(t *testing.T, set *fault.Set, s fault.Spec) *fault.Active {
	t.Helper()
	if err := s.Validate(fault.KindTCP); err != nil {
		t.Fatal(err)
	}
	a := fault.NewActive(s, time.Now(), 0)
	if err := set.Add(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func dial(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*net.TCPConn)
}

// rtts measures n request/response round trips of a 5 byte message.
func rtts(t *testing.T, c net.Conn, n int) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, n)
	buf := make([]byte, 5)
	for range n {
		start := time.Now()
		if _, err := c.Write([]byte("ping\n")); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		out = append(out, time.Since(start))
	}
	return out
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func TestPassthroughIntegrity(t *testing.T) {
	p, _ := startProxy(t, startEcho(t), 1)
	c := dial(t, p.Addr())
	payload := make([]byte, 1<<20)
	_, _ = rand.Read(payload)
	go func() {
		_, _ = c.Write(payload)
		_ = c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload corrupted: got %d bytes", len(got))
	}
	st := p.Stats()
	if st.BytesUpstream != int64(len(payload)) || st.BytesDownstream != int64(len(payload)) || st.Accepted != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if p.Upstream() == "" || p.Faults() == nil {
		t.Fatal("accessors")
	}
}

// TestLatencyAccuracy is the core accuracy check: the median added round
// trip latency must be within a few milliseconds of the configured value.
func TestLatencyAccuracy(t *testing.T) {
	echo := startEcho(t)
	p, set := startProxy(t, echo, 1)
	c := dial(t, p.Addr())
	base := median(rtts(t, c, 20))
	for _, want := range []time.Duration{20 * time.Millisecond, 60 * time.Millisecond} {
		a := addFault(t, set, fault.Spec{ID: "lat", Type: fault.Latency, Latency: fault.Duration(want)})
		got := median(rtts(t, c, 15)) - base
		set.Remove(a.ID)
		if got < want-2*time.Millisecond || got > want+15*time.Millisecond {
			t.Fatalf("configured %v, measured added latency %v", want, got)
		}
		t.Logf("configured %v measured %v (baseline %v)", want, got, base)
	}
}

func TestLatencyBothDirectionsAndJitterOrdering(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 3)
	addFault(t, set, fault.Spec{Type: fault.Latency, ID: "j", Latency: fault.Duration(20 * time.Millisecond),
		Jitter: fault.Duration(15 * time.Millisecond), Direction: fault.Both})
	c := dial(t, p.Addr())
	samples := rtts(t, c, 15)
	for _, s := range samples {
		// Two traversals of 20ms±15ms each: at least 10ms, at most ~70ms+slack.
		if s < 10*time.Millisecond || s > 120*time.Millisecond {
			t.Fatalf("rtt %v outside jitter envelope", s)
		}
	}
	// Ordering: stream many small writes and verify order on the way back.
	var want bytes.Buffer
	go func() {
		for i := range 200 {
			_, _ = c.Write([]byte{byte(i)})
		}
		_ = c.CloseWrite()
	}()
	for i := range 200 {
		want.WriteByte(byte(i))
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("jitter reordered bytes")
	}
}

func TestBandwidthAccuracy(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	const rate = 400 * 1024
	addFault(t, set, fault.Spec{Type: fault.Bandwidth, RateBPS: rate, Direction: fault.Downstream})
	c := dial(t, p.Addr())
	payload := make([]byte, 200*1024)
	start := time.Now()
	go func() {
		_, _ = c.Write(payload)
		_ = c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("read %d bytes: %v", len(got), err)
	}
	elapsed := time.Since(start)
	measured := float64(len(payload)) / elapsed.Seconds()
	if measured > rate*1.15 || measured < rate*0.7 {
		t.Fatalf("configured %d B/s, measured %.0f B/s over %v", rate, measured, elapsed)
	}
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func TestResetOnAccept(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.Reset})
	// The RST is sent right after accept, so on a loaded host it can reach
	// the client before connect() returns; a reset at dial time is the
	// same observation as a reset on the first read.
	c, err := net.DialTimeout("tcp", p.Addr(), 2*time.Second)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("hello"))
		_, err = c.Read(make([]byte, 8))
	}
	if !isReset(err) {
		t.Fatalf("want connection reset, got %v", err)
	}
	if p.Stats().Resets != 1 {
		t.Fatalf("resets = %d", p.Stats().Resets)
	}
}

func TestResetAfterDelay(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.Reset, Delay: fault.Duration(150 * time.Millisecond)})
	c := dial(t, p.Addr())
	start := time.Now()
	rtts(t, c, 1) // the connection works before the delay elapses
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 8))
	if !isReset(err) {
		t.Fatalf("want reset, got %v", err)
	}
	if el := time.Since(start); el < 140*time.Millisecond {
		t.Fatalf("reset after %v, before the configured delay", el)
	}
}

func TestResetAddedMidConnection(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	c := dial(t, p.Addr())
	rtts(t, c, 2)
	addFault(t, set, fault.Spec{Type: fault.Reset})
	_, _ = c.Write([]byte("ping\n"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 8))
	if err == nil {
		t.Fatal("expected the next chunk to trigger the reset")
	}
}

func TestCloseAfterBytes(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.Close, AfterBytes: 10})
	c := dial(t, p.Addr())
	_, _ = c.Write(bytes.Repeat([]byte("x"), 100))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil && !isReset(err) {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d bytes, want exactly 10 before close", len(got))
	}
	if p.Stats().Closes != 1 {
		t.Fatalf("closes = %d", p.Stats().Closes)
	}
}

func TestCloseOnAccept(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.Close})
	c := dial(t, p.Addr())
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("want immediate EOF, got n=%d err=%v", n, err)
	}
}

func TestBlackholeThenRecover(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	a := addFault(t, set, fault.Spec{Type: fault.Blackhole, Direction: fault.Upstream})
	c := dial(t, p.Addr())
	_, _ = c.Write([]byte("lost\n"))
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var ne net.Error
	if _, err := c.Read(make([]byte, 8)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want read timeout while blackholed, got %v", err)
	}
	if p.Stats().BytesDropped != 5 {
		t.Fatalf("dropped = %d", p.Stats().BytesDropped)
	}
	set.Remove(a.ID)
	if r := rtts(t, c, 1); r[0] > time.Second {
		t.Fatal("connection did not recover")
	}
}

func TestSlowClose(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.SlowClose, Delay: fault.Duration(200 * time.Millisecond), Direction: fault.Upstream})
	c := dial(t, p.Addr())
	rtts(t, c, 1)
	start := time.Now()
	_ = c.CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 190*time.Millisecond {
		t.Fatalf("close propagated after %v, want >= 200ms", el)
	}
}

func TestProbabilityIsPerConnection(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 42)
	addFault(t, set, fault.Spec{Type: fault.Close, Probability: fault.Float(0.5)})
	closed := 0
	const n = 200
	for range n {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("ping\n"))
		if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
			closed++
		}
		_ = c.Close()
	}
	if closed < n*35/100 || closed > n*65/100 {
		t.Fatalf("probability 0.5 closed %d/%d connections", closed, n)
	}
}

func TestMaxAffectedLimitsBlastRadius(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.Latency, Latency: fault.Duration(150 * time.Millisecond), MaxAffected: 1})
	c1 := dial(t, p.Addr())
	slow := rtts(t, c1, 1)[0] // c1 holds the only slot
	c2 := dial(t, p.Addr())
	fast := rtts(t, c2, 1)[0]
	if slow < 140*time.Millisecond || fast > 100*time.Millisecond {
		t.Fatalf("slow=%v fast=%v: max_affected not enforced", slow, fast)
	}
	_ = c1.Close()
	// Once c1 is gone its slot is released for new connections.
	deadline := time.Now().Add(2 * time.Second)
	for p.Stats().Active > 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c3 := dial(t, p.Addr())
	if r := rtts(t, c3, 1)[0]; r < 140*time.Millisecond {
		t.Fatalf("released slot not reused: rtt %v", r)
	}
}

func TestSourceScope(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 1)
	addFault(t, set, fault.Spec{Type: fault.Reset, Sources: []string{"10.0.0.0/8"}})
	c := dial(t, p.Addr())
	if r := rtts(t, c, 1)[0]; r > time.Second {
		t.Fatal("loopback client should be outside the fault scope")
	}
}

func TestDialFailure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	_ = ln.Close()
	set := fault.NewSet()
	p, err := Start(Config{Name: "dead", Listen: "127.0.0.1:0", Upstream: dead, Faults: set, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := dial(t, p.Addr())
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected connection to close")
	}
	if p.Stats().DialErrors != 1 {
		t.Fatalf("dial errors = %d", p.Stats().DialErrors)
	}
}

func TestStartErrorsAndClose(t *testing.T) {
	if _, err := Start(Config{Listen: "127.0.0.1:0"}); err == nil {
		t.Fatal("missing upstream accepted")
	}
	if _, err := Start(Config{Listen: "256.0.0.1:0", Upstream: "127.0.0.1:1"}); err == nil {
		t.Fatal("bad listen accepted")
	}
	p, _ := startProxy(t, startEcho(t), 1)
	c := dial(t, p.Addr())
	rtts(t, c, 1)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("Close must tear down active sessions")
	}
}

func TestPacerRate(t *testing.T) {
	var p pacer
	var buf bytes.Buffer
	start := time.Now()
	if err := p.write(t.Context(), &buf, make([]byte, 10000), 50000); err != nil {
		t.Fatal(err)
	}
	// 10000 bytes at 50000 B/s: 0.2s, minus the first slice which is free.
	if el := time.Since(start); el < 150*time.Millisecond || el > 400*time.Millisecond {
		t.Fatalf("paced write took %v", el)
	}
	if buf.Len() != 10000 {
		t.Fatal("short write")
	}
}

func TestResetProbabilityAndCap(t *testing.T) {
	p, set := startProxy(t, startEcho(t), 42)
	addFault(t, set, fault.Spec{Type: fault.Reset, Probability: fault.Float(0.3), MaxAffected: 2})
	reset := 0
	const n = 200
	for range n {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			reset++ // reset before the handshake completed on the client side
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("ping\n"))
		if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
			reset++
		}
		_ = c.Close()
	}
	t.Logf("probability 0.3 reset %d/%d connections", reset, n)
	if reset < n*20/100 || reset > n*40/100 {
		t.Fatalf("probability 0.3 reset %d/%d connections", reset, n)
	}
}
