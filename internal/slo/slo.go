// Package slo summarises probe samples (success rate, latency percentiles,
// error breakdown) and evaluates them against service level objectives.
package slo

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/archit2501/faultline/internal/fault"
	"github.com/archit2501/faultline/internal/probe"
)

// Objective is a service level objective for one probe. Unset fields are
// not checked; at least one must be set.
type Objective struct {
	Probe          string         `json:"probe"`
	MinSuccessRate *float64       `json:"min_success_rate,omitempty"`
	MaxP50         fault.Duration `json:"max_p50,omitempty"`
	MaxP95         fault.Duration `json:"max_p95,omitempty"`
	MaxP99         fault.Duration `json:"max_p99,omitempty"`
}

// Validate checks the objective's bounds.
func (o Objective) Validate() error {
	if o.Probe == "" {
		return errors.New("objective needs a probe")
	}
	if o.MinSuccessRate == nil && o.MaxP50 == 0 && o.MaxP95 == 0 && o.MaxP99 == 0 {
		return fmt.Errorf("objective for %q sets no threshold", o.Probe)
	}
	if r := o.MinSuccessRate; r != nil && (*r < 0 || *r > 1) {
		return fmt.Errorf("objective for %q: min_success_rate must be within [0,1]", o.Probe)
	}
	if o.MaxP50 < 0 || o.MaxP95 < 0 || o.MaxP99 < 0 {
		return fmt.Errorf("objective for %q: latency bounds must not be negative", o.Probe)
	}
	return nil
}

// Stats summarises a set of samples of one probe.
type Stats struct {
	Count       int            `json:"count"`
	Successes   int            `json:"successes"`
	SuccessRate float64        `json:"success_rate"`
	P50         time.Duration  `json:"p50_ns"`
	P95         time.Duration  `json:"p95_ns"`
	P99         time.Duration  `json:"p99_ns"`
	Max         time.Duration  `json:"max_ns"`
	Mean        time.Duration  `json:"mean_ns"`
	Errors      map[string]int `json:"errors,omitempty"`
}

// Summarize computes statistics in O(n log n). Latency percentiles include
// failed samples: a request that timed out after 2s was a 2s request from
// the caller's point of view.
func Summarize(samples []probe.Sample) Stats {
	st := Stats{Count: len(samples)}
	if len(samples) == 0 {
		return st
	}
	lat := make([]time.Duration, len(samples))
	var sum time.Duration
	for i, s := range samples {
		lat[i] = s.Latency
		sum += s.Latency
		if s.OK {
			st.Successes++
		} else {
			if st.Errors == nil {
				st.Errors = map[string]int{}
			}
			st.Errors[s.Err]++
		}
	}
	slices.Sort(lat)
	st.SuccessRate = float64(st.Successes) / float64(st.Count)
	st.P50 = Percentile(lat, 0.50)
	st.P95 = Percentile(lat, 0.95)
	st.P99 = Percentile(lat, 0.99)
	st.Max = lat[len(lat)-1]
	st.Mean = sum / time.Duration(len(lat))
	return st
}

// Percentile returns the nearest-rank percentile of sorted values.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	idx = max(0, min(idx, len(sorted)-1))
	return sorted[idx]
}

// Check is the outcome of one threshold comparison.
type Check struct {
	Probe  string `json:"probe"`
	Metric string `json:"metric"`
	Want   string `json:"want"`
	Got    string `json:"got"`
	Pass   bool   `json:"pass"`
}

func (c Check) String() string {
	verdict := "ok"
	if !c.Pass {
		verdict = "BREACH"
	}
	return fmt.Sprintf("%s %s %s (got %s) %s", c.Probe, c.Metric, c.Want, c.Got, verdict)
}

// Evaluate compares stats against an objective. With no samples every
// check fails: an objective that cannot be observed is not met.
func Evaluate(o Objective, st Stats) []Check {
	var out []Check
	if r := o.MinSuccessRate; r != nil {
		out = append(out, Check{
			Probe: o.Probe, Metric: "success_rate",
			Want: fmt.Sprintf(">= %.3f", *r), Got: fmt.Sprintf("%.3f (%d/%d)", st.SuccessRate, st.Successes, st.Count),
			Pass: st.Count > 0 && st.SuccessRate >= *r,
		})
	}
	lat := func(metric string, limit fault.Duration, got time.Duration) {
		if limit == 0 {
			return
		}
		out = append(out, Check{
			Probe: o.Probe, Metric: metric,
			Want: "<= " + limit.String(), Got: Round(got).String(),
			Pass: st.Count > 0 && got <= limit.D(),
		})
	}
	lat("p50", o.MaxP50, st.P50)
	lat("p95", o.MaxP95, st.P95)
	lat("p99", o.MaxP99, st.P99)
	return out
}

// AllPass reports whether every check passed.
func AllPass(checks []Check) bool {
	for _, c := range checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

// Round trims a duration to a readable precision for reports.
func Round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

// Bucket summarises samples in one time slice of a timeline.
type Bucket struct {
	Offset time.Duration `json:"offset_ns"`
	Count  int           `json:"count"`
	Errors int           `json:"errors"`
	P95    time.Duration `json:"p95_ns"`
}

// Buckets slices samples into fixed-width windows starting at start, in
// O(n log n). Empty windows are kept so the timeline has no gaps.
func Buckets(samples []probe.Sample, start time.Time, end time.Time, width time.Duration) []Bucket {
	if width <= 0 || !end.After(start) {
		return nil
	}
	n := int((end.Sub(start) + width - 1) / width)
	lat := make([][]time.Duration, n)
	out := make([]Bucket, n)
	for i := range out {
		out[i].Offset = time.Duration(i) * width
	}
	for _, s := range samples {
		if s.At.Before(start) || !s.At.Before(end) {
			continue
		}
		i := int(s.At.Sub(start) / width)
		out[i].Count++
		if !s.OK {
			out[i].Errors++
		}
		lat[i] = append(lat[i], s.Latency)
	}
	for i := range out {
		slices.Sort(lat[i])
		out[i].P95 = Percentile(lat[i], 0.95)
	}
	return out
}
