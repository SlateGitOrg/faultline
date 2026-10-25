package slo

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/fault"
	"github.com/archit2501/faultline/internal/probe"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func samples(lat []int, okEvery int) []probe.Sample {
	base := time.Unix(1000, 0)
	out := make([]probe.Sample, len(lat))
	for i, l := range lat {
		ok := okEvery == 0 || i%okEvery != 0
		s := probe.Sample{Probe: "p", At: base.Add(ms(i * 100)), Latency: ms(l), OK: ok}
		if !ok {
			s.Err = "timeout"
		}
		out[i] = s
	}
	return out
}

func TestSummarize(t *testing.T) {
	var lat []int
	for i := 1; i <= 100; i++ {
		lat = append(lat, i)
	}
	st := Summarize(samples(lat, 10))
	if st.Count != 100 || st.Successes != 90 || st.SuccessRate != 0.9 || st.Errors["timeout"] != 10 {
		t.Fatalf("%+v", st)
	}
	if st.P50 != ms(50) || st.P95 != ms(95) || st.P99 != ms(99) || st.Max != ms(100) || st.Mean != ms(50)+500*time.Microsecond {
		t.Fatalf("percentiles %+v", st)
	}
	if z := Summarize(nil); z.Count != 0 || z.P95 != 0 {
		t.Fatal("empty")
	}
	if Percentile(nil, 0.5) != 0 || Percentile([]time.Duration{7}, 0.99) != 7 || Percentile([]time.Duration{1, 2}, 0) != 1 {
		t.Fatal("percentile edges")
	}
}

// TestPercentileProperties checks invariants on random inputs: percentiles
// are ordered, bounded by min/max and are members of the input.
func TestPercentileProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		n := 1 + r.IntN(200)
		xs := make([]time.Duration, n)
		for i := range xs {
			xs[i] = time.Duration(r.Int64N(1e9))
		}
		slices.Sort(xs)
		p50, p95, p99 := Percentile(xs, .5), Percentile(xs, .95), Percentile(xs, .99)
		if p50 > p95 || p95 > p99 || p99 > xs[n-1] || p50 < xs[0] {
			t.Fatalf("ordering violated: %v %v %v", p50, p95, p99)
		}
		if _, found := slices.BinarySearch(xs, p95); !found {
			t.Fatal("nearest-rank percentile must be an observed value")
		}
		// At least p of the values are <= the p-th percentile.
		le := 0
		for _, x := range xs {
			if x <= p95 {
				le++
			}
		}
		if float64(le) < 0.95*float64(n) {
			t.Fatalf("only %d/%d values <= p95", le, n)
		}
	}
}

func rate(v float64) *float64 { return &v }

func TestEvaluate(t *testing.T) {
	st := Summarize(samples([]int{10, 20, 30, 40, 200}, 0))
	o := Objective{Probe: "p", MinSuccessRate: rate(0.99), MaxP50: fault.Duration(ms(50)), MaxP95: fault.Duration(ms(100)), MaxP99: fault.Duration(ms(500))}
	checks := Evaluate(o, st)
	if len(checks) != 4 {
		t.Fatalf("checks: %v", checks)
	}
	want := map[string]bool{"success_rate": true, "p50": true, "p95": false, "p99": true}
	for _, c := range checks {
		if c.Pass != want[c.Metric] {
			t.Fatalf("%s pass=%v", c, c.Pass)
		}
	}
	if AllPass(checks) {
		t.Fatal("AllPass with a breach")
	}
	if !strings.Contains(checks[2].String(), "BREACH") || !strings.Contains(checks[0].String(), "ok") {
		t.Fatalf("String: %s / %s", checks[2], checks[0])
	}
	empty := Evaluate(Objective{Probe: "p", MinSuccessRate: rate(0)}, Stats{})
	if empty[0].Pass {
		t.Fatal("no samples must fail the objective")
	}
	if !AllPass(nil) {
		t.Fatal("no checks pass trivially")
	}
}

func TestObjectiveValidate(t *testing.T) {
	bad := []Objective{
		{},
		{Probe: "p"},
		{Probe: "p", MinSuccessRate: rate(2)},
		{Probe: "p", MaxP95: -1},
	}
	for _, o := range bad {
		if o.Validate() == nil {
			t.Fatalf("%+v accepted", o)
		}
	}
	if (Objective{Probe: "p", MaxP99: 1}).Validate() != nil {
		t.Fatal("valid rejected")
	}
}

func TestBuckets(t *testing.T) {
	ss := samples([]int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120}, 4)
	start := ss[0].At
	b := Buckets(ss, start, start.Add(1500*time.Millisecond), 500*time.Millisecond)
	if len(b) != 3 {
		t.Fatalf("len %d", len(b))
	}
	if b[0].Count != 5 || b[1].Count != 5 || b[2].Count != 2 {
		t.Fatalf("counts %+v", b)
	}
	if b[0].Errors != 2 || b[0].P95 != ms(50) || b[1].Offset != 500*time.Millisecond {
		t.Fatalf("bucket0 %+v bucket1 %+v", b[0], b[1])
	}
	gap := Buckets(ss[:1], start, start.Add(time.Second), 250*time.Millisecond)
	if len(gap) != 4 || gap[1].Count != 0 || gap[1].P95 != 0 {
		t.Fatalf("empty buckets must be kept: %+v", gap)
	}
	if Buckets(ss, start, start, time.Second) != nil || Buckets(ss, start, start.Add(time.Second), 0) != nil {
		t.Fatal("degenerate windows")
	}
}

func TestRound(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		1234567891:             1235 * time.Millisecond,
		12345678:               12300 * time.Microsecond,
		123456:                 123 * time.Microsecond,
		1500 * time.Nanosecond: 2 * time.Microsecond,
	}
	for in, want := range cases {
		if got := Round(in); got != want {
			t.Fatalf("Round(%v)=%v want %v", in, got, want)
		}
	}
}
