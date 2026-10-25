// Command latencybench measures how accurately faultline injects what it is
// asked to inject, against in-process loopback servers:
//
//   - L4 latency: added round-trip latency through the TCP proxy for a range
//     of configured latencies (one direction, so one traversal per RTT);
//   - L4 jitter: the spread of a latency±jitter fault versus the uniform
//     distribution it should follow;
//   - L7 delay: added request latency through the HTTP proxy;
//   - L4 bandwidth: achieved throughput for a range of rate caps;
//   - proxy overhead: round trip and bulk throughput with no faults;
//   - host timer overshoot: how late a bare time.Timer of the same duration
//     fires, which bounds the accuracy any user-space injector can reach.
//
// It prints Markdown tables (the README's accuracy section is produced by
// `make bench`). Numbers depend on the machine and its load.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"time"

	"github.com/SlateGitOrg/faultline/internal/fault"
	"github.com/SlateGitOrg/faultline/internal/httpproxy"
	"github.com/SlateGitOrg/faultline/internal/tcpproxy"
)

type latencyRow struct {
	Layer       string  `json:"layer"`
	ConfiguredM float64 `json:"configured_ms"`
	Samples     int     `json:"samples"`
	MedianAdded float64 `json:"median_added_ms"`
	P99Added    float64 `json:"p99_added_ms"`
	MedianError float64 `json:"median_error_ms"`
	TimerSlack  float64 `json:"host_timer_overshoot_ms"`
}

type bandwidthRow struct {
	ConfiguredBPS int64   `json:"configured_bps"`
	Bytes         int     `json:"bytes"`
	MeasuredBPS   float64 `json:"measured_bps"`
	ErrorPct      float64 `json:"error_pct"`
}

type jitterRow struct {
	LatencyMS, JitterMS  float64
	Samples              int
	MinMS, MaxMS, MeanMS float64
	StdMS, ExpectedStdMS float64
}

type results struct {
	GoVersion  string         `json:"go_version"`
	OS         string         `json:"os"`
	CPUs       int            `json:"cpus"`
	BaseRTTUS  float64        `json:"direct_rtt_median_us"`
	ProxyRTTUS float64        `json:"proxied_rtt_median_us"`
	DirectMBps float64        `json:"direct_bulk_mbps"`
	ProxyMBps  float64        `json:"proxied_bulk_mbps"`
	Latency    []latencyRow   `json:"latency"`
	Jitter     jitterRow      `json:"jitter"`
	Bandwidth  []bandwidthRow `json:"bandwidth"`
}

func main() {
	quick := flag.Bool("quick", false, "fewer samples and configurations (smoke run)")
	asJSON := flag.Bool("json", false, "print JSON instead of Markdown")
	flag.Parse()
	res, err := run(*quick)
	if err != nil {
		fmt.Fprintln(os.Stderr, "latencybench:", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}
	printMarkdown(res)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func run(quick bool) (*results, error) {
	res := &results{GoVersion: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH, CPUs: runtime.NumCPU()}
	echo, err := startEcho()
	if err != nil {
		return nil, err
	}
	defer echo.Close()
	set := fault.NewSet()
	tp, err := tcpproxy.Start(tcpproxy.Config{Name: "bench", Listen: "127.0.0.1:0", Upstream: echo.Addr().String(), Faults: set, RNG: fault.NewRNG(1)})
	if err != nil {
		return nil, err
	}
	defer tp.Close()

	direct, err := dial(echo.Addr().String())
	if err != nil {
		return nil, err
	}
	defer direct.Close()
	proxied, err := dial(tp.Addr())
	if err != nil {
		return nil, err
	}
	defer proxied.Close()

	warm := 300
	if quick {
		warm = 50
	}
	// Warm both paths (scheduler, caches, TCP state) before measuring either.
	for _, c := range []net.Conn{direct, proxied} {
		if _, err := rtts(c, warm); err != nil {
			return nil, err
		}
	}
	baseRTT, err := rtts(direct, warm)
	if err != nil {
		return nil, err
	}
	proxyRTT, err := rtts(proxied, warm)
	if err != nil {
		return nil, err
	}
	res.BaseRTTUS = float64(median(baseRTT)) / float64(time.Microsecond)
	res.ProxyRTTUS = float64(median(proxyRTT)) / float64(time.Microsecond)
	base := median(proxyRTT) // added latency is measured against the proxy's own no-fault RTT

	configs := []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond}
	if quick {
		configs = []time.Duration{5 * time.Millisecond, 50 * time.Millisecond}
	}
	slack := map[time.Duration]float64{}
	for _, d := range configs {
		slack[d] = ms(timerOvershoot(d, min(samplesFor(d, quick), 40)))
	}
	for _, want := range configs {
		n := samplesFor(want, quick)
		var row latencyRow
		if want == 0 {
			row, err = latencyRowOf("L4 tcp", want, proxyRTT[:min(n, len(proxyRTT))], base)
		} else {
			row, err = measureTCP(set, proxied, want, n, base)
		}
		if err != nil {
			return nil, err
		}
		row.TimerSlack = slack[want]
		res.Latency = append(res.Latency, row)
	}

	res.Jitter, err = measureJitter(set, proxied, quick)
	if err != nil {
		return nil, err
	}

	httpRows, err := measureHTTP(configs, quick)
	if err != nil {
		return nil, err
	}
	for i := range httpRows {
		httpRows[i].TimerSlack = slack[time.Duration(httpRows[i].ConfiguredM*float64(time.Millisecond))]
	}
	res.Latency = append(res.Latency, httpRows...)

	res.DirectMBps, res.ProxyMBps, err = bulk(echo.Addr().String(), tp.Addr(), quick)
	if err != nil {
		return nil, err
	}

	rates := []int64{16 << 10, 64 << 10, 256 << 10, 1 << 20}
	if quick {
		rates = []int64{256 << 10}
	}
	for _, r := range rates {
		row, err := measureBandwidth(set, tp.Addr(), r)
		if err != nil {
			return nil, err
		}
		res.Bandwidth = append(res.Bandwidth, row)
	}
	return res, nil
}

// timerOvershoot is the median lateness of a bare timer of duration d.
func timerOvershoot(d time.Duration, n int) time.Duration {
	if d == 0 {
		return 0
	}
	late := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		t := time.NewTimer(d)
		<-t.C
		late = append(late, time.Since(start)-d)
	}
	return median(late)
}

// samplesFor keeps each configuration to roughly three seconds.
func samplesFor(d time.Duration, quick bool) int {
	if quick {
		return 20
	}
	if d == 0 {
		return 200
	}
	return max(30, min(200, int(3*time.Second/d)))
}

func latencyRowOf(layer string, want time.Duration, samples []time.Duration, base time.Duration) (latencyRow, error) {
	if len(samples) == 0 {
		return latencyRow{}, fmt.Errorf("%s %v: no samples", layer, want)
	}
	added := make([]time.Duration, len(samples))
	for i, s := range samples {
		added[i] = s - base
	}
	slices.Sort(added)
	med := added[len(added)/2]
	p99 := added[min(len(added)-1, int(math.Ceil(0.99*float64(len(added))))-1)]
	return latencyRow{
		Layer: layer, ConfiguredM: ms(want), Samples: len(samples),
		MedianAdded: ms(med), P99Added: ms(p99), MedianError: ms(med - want),
	}, nil
}

func measureTCP(set *fault.Set, c net.Conn, want time.Duration, n int, base time.Duration) (latencyRow, error) {
	a, err := install(set, fault.KindTCP, fault.Spec{ID: "lat", Type: fault.Latency, Latency: fault.Duration(want)})
	if err != nil {
		return latencyRow{}, err
	}
	defer set.Remove(a.ID)
	samples, err := rtts(c, n)
	if err != nil {
		return latencyRow{}, err
	}
	return latencyRowOf("L4 tcp", want, samples, base)
}

func measureJitter(set *fault.Set, c net.Conn, quick bool) (jitterRow, error) {
	lat, jit := 50*time.Millisecond, 20*time.Millisecond
	n := 150
	if quick {
		n = 30
	}
	base, err := rtts(c, 50)
	if err != nil {
		return jitterRow{}, err
	}
	b := median(base)
	a, err := install(set, fault.KindTCP, fault.Spec{ID: "jit", Type: fault.Latency, Latency: fault.Duration(lat), Jitter: fault.Duration(jit)})
	if err != nil {
		return jitterRow{}, err
	}
	defer set.Remove(a.ID)
	// One message at a time, so the never-reorder clamp does not apply.
	samples, err := rtts(c, n)
	if err != nil {
		return jitterRow{}, err
	}
	row := jitterRow{LatencyMS: ms(lat), JitterMS: ms(jit), Samples: n, MinMS: math.Inf(1), ExpectedStdMS: ms(jit) / math.Sqrt(3)}
	var sum, sq float64
	for _, s := range samples {
		v := ms(s - b)
		sum += v
		sq += v * v
		row.MinMS = math.Min(row.MinMS, v)
		row.MaxMS = math.Max(row.MaxMS, v)
	}
	row.MeanMS = sum / float64(n)
	row.StdMS = math.Sqrt(sq/float64(n) - row.MeanMS*row.MeanMS)
	return row, nil
}

func measureHTTP(configs []time.Duration, quick bool) ([]latencyRow, error) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	set := fault.NewSet()
	hp, err := httpproxy.Start(httpproxy.Config{Name: "bench", Listen: "127.0.0.1:0", Upstream: up.URL, Faults: set, RNG: fault.NewRNG(1)})
	if err != nil {
		return nil, err
	}
	defer hp.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	url := "http://" + hp.Addr() + "/"
	get := func(n int) ([]time.Duration, error) {
		out := make([]time.Duration, 0, n)
		for range n {
			start := time.Now()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			out = append(out, time.Since(start))
		}
		return out, nil
	}
	warm := 200
	if quick {
		warm = 30
	}
	baseline, err := get(warm)
	if err != nil {
		return nil, err
	}
	base := median(baseline)
	var rows []latencyRow
	for _, want := range configs {
		n := samplesFor(want, quick)
		samples := baseline[:min(n, len(baseline))]
		if want > 0 {
			a, err := install(set, fault.KindHTTP, fault.Spec{ID: "delay", Type: fault.Delay, Latency: fault.Duration(want)})
			if err != nil {
				return nil, err
			}
			samples, err = get(n)
			set.Remove(a.ID)
			if err != nil {
				return nil, err
			}
		}
		row, err := latencyRowOf("L7 http", want, samples, base)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func measureBandwidth(set *fault.Set, addr string, rate int64) (bandwidthRow, error) {
	a, err := install(set, fault.KindTCP, fault.Spec{ID: "bw", Type: fault.Bandwidth, RateBPS: rate})
	if err != nil {
		return bandwidthRow{}, err
	}
	defer set.Remove(a.ID)
	size := int(rate * 2) // two seconds of transfer at the cap
	elapsed, err := transfer(addr, size)
	if err != nil {
		return bandwidthRow{}, err
	}
	got := float64(size) / elapsed.Seconds()
	return bandwidthRow{ConfiguredBPS: rate, Bytes: size, MeasuredBPS: got, ErrorPct: 100 * (got - float64(rate)) / float64(rate)}, nil
}

func bulk(direct, proxied string, quick bool) (float64, float64, error) {
	size := 256 << 20
	if quick {
		size = 32 << 20
	}
	d, err := transfer(direct, size)
	if err != nil {
		return 0, 0, err
	}
	p, err := transfer(proxied, size)
	if err != nil {
		return 0, 0, err
	}
	mb := float64(size) / (1 << 20)
	return mb / d.Seconds(), mb / p.Seconds(), nil
}

// transfer echoes size bytes through addr and returns the elapsed time.
func transfer(addr string, size int) (time.Duration, error) {
	c, err := dial(addr)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		left := size
		for left > 0 {
			n, err := c.Write(buf[:min(left, len(buf))])
			if err != nil {
				errc <- err
				return
			}
			left -= n
		}
		errc <- c.CloseWrite()
	}()
	n, err := io.Copy(io.Discard, c)
	if err != nil {
		return 0, err
	}
	if werr := <-errc; werr != nil {
		return 0, werr
	}
	if int(n) != size {
		return 0, fmt.Errorf("echoed %d of %d bytes", n, size)
	}
	return time.Since(start), nil
}

func install(set *fault.Set, kind fault.Kind, s fault.Spec) (*fault.Active, error) {
	if err := s.Validate(kind); err != nil {
		return nil, err
	}
	a := fault.NewActive(s, time.Now(), 0)
	return a, set.Add(a)
}

func startEcho() (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return ln, nil
}

func dial(addr string) (*net.TCPConn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	return c.(*net.TCPConn), nil
}

// rtts measures n round trips of a 5 byte message.
func rtts(c net.Conn, n int) ([]time.Duration, error) {
	out := make([]time.Duration, 0, n)
	buf := make([]byte, 5)
	for range n {
		start := time.Now()
		if _, err := c.Write([]byte("ping\n")); err != nil {
			return nil, err
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			return nil, err
		}
		out = append(out, time.Since(start))
	}
	return out, nil
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

func printMarkdown(r *results) {
	fmt.Printf("Environment: %s, %s, %d CPUs. All traffic is loopback.\n\n", r.GoVersion, r.OS, r.CPUs)
	fmt.Printf("Proxy overhead (no faults): median RTT direct %.0fµs vs proxied %.0fµs; bulk echo throughput direct %.0f MB/s vs proxied %.0f MB/s.\n\n",
		r.BaseRTTUS, r.ProxyRTTUS, r.DirectMBps, r.ProxyMBps)
	fmt.Println("Injected latency accuracy (added = measured − no-fault median through the same proxy):")
	fmt.Println()
	fmt.Println("| Layer | Configured | Samples | Median added | p99 added | Median error | Host timer overshoot |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|")
	for _, l := range r.Latency {
		fmt.Printf("| %s | %.0f ms | %d | %.2f ms | %.2f ms | %+.2f ms | %+.2f ms |\n", l.Layer, l.ConfiguredM, l.Samples, l.MedianAdded, l.P99Added, l.MedianError, l.TimerSlack)
	}
	j := r.Jitter
	fmt.Printf("\nJitter (L4, %.0f ms ± %.0f ms, %d samples): added latency min %.1f / mean %.1f / max %.1f ms; std dev %.2f ms (uniform expectation %.2f ms).\n\n",
		j.LatencyMS, j.JitterMS, j.Samples, j.MinMS, j.MeanMS, j.MaxMS, j.StdMS, j.ExpectedStdMS)
	fmt.Println("Bandwidth cap accuracy (two seconds of transfer at each cap):")
	fmt.Println()
	fmt.Println("| Configured | Bytes | Measured | Error |")
	fmt.Println("|---:|---:|---:|---:|")
	for _, b := range r.Bandwidth {
		fmt.Printf("| %d KiB/s | %d | %.1f KiB/s | %+.1f%% |\n", b.ConfiguredBPS>>10, b.Bytes, b.MeasuredBPS/1024, b.ErrorPct)
	}
}
