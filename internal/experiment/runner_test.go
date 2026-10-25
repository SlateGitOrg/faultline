package experiment

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/control"
	"github.com/archit2501/faultline/internal/engine"
	"github.com/archit2501/faultline/internal/fault"
	"github.com/archit2501/faultline/internal/probe"
	"github.com/archit2501/faultline/internal/report"
	"github.com/archit2501/faultline/internal/sample"
)

// stack is the full system under test, in process: a faultline engine
// behind its real control API, the sample pricing backend, a TCP proxy in
// front of it and the catalog API calling pricing through that proxy.
type stack struct {
	eng     *engine.Engine
	client  *control.Client
	pricing *sample.Pricing
	catalog *sample.Catalog
	env     map[string]string
}

func newStack(t *testing.T, opts engine.Options) *stack {
	t.Helper()
	opts.Seed = 7
	eng := engine.New(opts)
	t.Cleanup(func() { _ = eng.Close() })
	srv := httptest.NewServer(control.NewHandler(eng, control.HandlerOptions{}))
	t.Cleanup(srv.Close)
	pricing, err := sample.StartPricing("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pricing.Close() })
	pi, err := eng.CreateProxy(engine.ProxySpec{Name: "pricing", Kind: fault.KindTCP, Upstream: pricing.Addr()})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := sample.StartCatalog(sample.CatalogConfig{Listen: "127.0.0.1:0", PricingAddr: pi.Listen, PricingTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	return &stack{
		eng: eng, client: control.NewClient(srv.URL, ""), pricing: pricing, catalog: catalog,
		env: map[string]string{"CTL": srv.URL, "CATALOG": catalog.Addr()},
	}
}

func (s *stack) run(t *testing.T, ctx context.Context, doc string) *report.Report {
	t.Helper()
	spec, err := Parse([]byte(doc), env(s.env))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Control: s.client, IsNotFound: control.IsNotFound}
	rep := r.Run(ctx, spec)
	var md strings.Builder
	rep.Markdown(&md)
	t.Log("\n" + md.String())
	return rep
}

func (s *stack) faultCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, p := range s.eng.Proxies() {
		n += len(p.Faults)
	}
	return n
}

const header = `
name: %s
control: ${CTL}
proxies:
  - name: edge
    kind: http
    upstream: http://${CATALOG}
probes:
  - name: catalog
    timeout: 1s
    http:
      url: proxy://edge/products
      body_contains: anvil
steady_state:
  duration: 600ms
  interval: 50ms
  objectives:
    - probe: catalog
      min_success_rate: 0.95
      max_p95: 120ms
`

func TestPassedWhenSystemTolerates(t *testing.T) {
	s := newStack(t, engine.Options{})
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "tolerate")+`
method:
  - name: mild-latency
    proxy: pricing
    duration: 800ms
    faults:
      - type: latency
        latency: 30ms
        jitter: 5ms
`)
	if rep.Verdict != report.Passed || rep.Hypothesis != "held" || rep.Recovered == nil || !*rep.Recovered {
		t.Fatalf("verdict=%s hypothesis=%s errors=%v", rep.Verdict, rep.Hypothesis, rep.Errors)
	}
	if len(rep.Injections) != 1 || rep.Injections[0].RemovedBy != "schedule" {
		t.Fatalf("injections %+v", rep.Injections)
	}
	method := rep.Phases[1]
	if method.Name != PhaseMethod || method.Probes[0].P50 < 30*time.Millisecond {
		t.Fatalf("injected latency not visible in method phase: %+v", method.Probes[0])
	}
	if s.faultCount(t) != 0 {
		t.Fatal("faults left behind")
	}
	if _, err := s.eng.Proxy("edge"); err == nil {
		t.Fatal("proxy created by the experiment was not torn down")
	}
	if len(rep.Buckets["catalog"]) == 0 || rep.BucketWidth == 0 {
		t.Fatal("timeline buckets missing")
	}
}

// TestAbortOnSLOBreach is the core safety property: a fault that breaks
// the abort objective is rolled back long before its scheduled end.
func TestAbortOnSLOBreach(t *testing.T) {
	s := newStack(t, engine.Options{})
	start := time.Now()
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "breach")+`
method:
  - name: pricing-outage-latency
    proxy: pricing
    duration: 30s
    faults:
      - type: latency
        latency: 400ms
abort:
  window: 500ms
  min_samples: 4
  objectives:
    - probe: catalog
      min_success_rate: 0.8
`)
	elapsed := time.Since(start)
	if rep.Verdict != report.Aborted || !strings.Contains(rep.AbortReason, "success_rate") {
		t.Fatalf("verdict=%s reason=%q", rep.Verdict, rep.AbortReason)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("abort took %v; the step was scheduled for 30s", elapsed)
	}
	if len(rep.AbortChecks) == 0 || rep.AbortChecks[0].Pass {
		t.Fatalf("abort checks %+v", rep.AbortChecks)
	}
	if in := rep.Injections[0]; in.RemovedBy != "rollback" || in.RemovedAt == nil {
		t.Fatalf("injection not rolled back: %+v", in)
	}
	if rep.Recovered == nil || !*rep.Recovered {
		t.Fatal("system should recover once the latency is rolled back")
	}
	if s.faultCount(t) != 0 {
		t.Fatal("faults left behind after abort")
	}
	// Time from injection to abort is bounded by window + check interval
	// + a probe timeout, not by the 30s schedule.
	var abortAt time.Time
	for _, ev := range rep.Timeline {
		if ev.Kind == "abort" {
			abortAt = ev.At
		}
	}
	if d := abortAt.Sub(rep.Injections[0].InjectedAt); d <= 0 || d > 3*time.Second {
		t.Fatalf("abort %v after injection", d)
	}
}

func TestFailedWhenHypothesisDeviates(t *testing.T) {
	s := newStack(t, engine.Options{})
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "deviate")+`
method:
  - name: edge-errors
    proxy: edge
    duration: 800ms
    faults:
      - type: status
        status: 503
        probability: 0.5
`)
	if rep.Verdict != report.Failed || rep.Hypothesis != "deviated" || !*rep.Recovered {
		t.Fatalf("verdict=%s hypothesis=%s", rep.Verdict, rep.Hypothesis)
	}
	if rep.Phases[1].Probes[0].Errors["status 503"] == 0 {
		t.Fatalf("no injected 503s observed: %+v", rep.Phases[1].Probes[0])
	}
}

func TestPreconditionFailedInjectsNothing(t *testing.T) {
	s := newStack(t, engine.Options{})
	_ = s.pricing.Close() // the system is already broken
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "precondition")+`
method:
  - proxy: pricing
    duration: 1s
    faults:
      - type: reset
`)
	if rep.Verdict != report.PreconditionFailed || len(rep.Injections) != 0 {
		t.Fatalf("verdict=%s injections=%v", rep.Verdict, rep.Injections)
	}
	if rep.Verdict.ExitCode() != 4 {
		t.Fatal("exit code")
	}
}

func TestSetupErrors(t *testing.T) {
	s := newStack(t, engine.Options{})
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "ghost-step")+`
method:
  - proxy: ghost
    duration: 1s
    faults:
      - type: reset
`)
	if rep.Verdict != report.Errored || !strings.Contains(strings.Join(rep.Errors, " "), "ghost") {
		t.Fatalf("verdict=%s errors=%v", rep.Verdict, rep.Errors)
	}
	// A declared proxy that exists with a different kind is refused.
	rep = s.run(t, t.Context(), strings.NewReplacer("name: edge\n    kind: http", "name: pricing\n    kind: http", "proxy://edge", "proxy://pricing").Replace(fmt.Sprintf(header, "kind-clash"))+`
method:
  - proxy: pricing
    duration: 1s
    faults:
      - type: abort
`)
	if rep.Verdict != report.Errored || !strings.Contains(strings.Join(rep.Errors, " "), "exists with kind tcp") {
		t.Fatalf("verdict=%s errors=%v", rep.Verdict, rep.Errors)
	}
	// Proxy creation blocked by the engine's allowlist.
	s.env["CATALOG"] = "10.9.9.9:80"
	rep = s.run(t, t.Context(), fmt.Sprintf(header, "blocked")+`
method:
  - proxy: edge
    duration: 1s
    faults:
      - type: abort
`)
	if rep.Verdict != report.Errored || !strings.Contains(strings.Join(rep.Errors, " "), "allowlist") {
		t.Fatalf("verdict=%s errors=%v", rep.Verdict, rep.Errors)
	}
}

func TestInjectionRejectedAborts(t *testing.T) {
	// The engine refuses TTLs above 1s; the step needs duration+grace.
	s := newStack(t, engine.Options{MaxTTL: time.Second})
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "rejected")+`
method:
  - name: long
    proxy: pricing
    duration: 2s
    faults:
      - type: latency
        latency: 10ms
`)
	if rep.Verdict != report.Aborted || !strings.Contains(rep.AbortReason, "injection failed") {
		t.Fatalf("verdict=%s reason=%q", rep.Verdict, rep.AbortReason)
	}
	if rep.Injections[0].Error == "" || !strings.Contains(rep.Injections[0].Error, "403") {
		t.Fatalf("injection error %+v", rep.Injections[0])
	}
}

func TestInterruptRollsBack(t *testing.T) {
	s := newStack(t, engine.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		// Cancel once the fault is in place.
		for s.faultCount(t) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	rep := s.run(t, ctx, fmt.Sprintf(header, "interrupt")+`
method:
  - proxy: pricing
    duration: 30s
    faults:
      - type: latency
        latency: 5ms
`)
	if rep.Verdict != report.Aborted || rep.AbortReason != "interrupted" {
		t.Fatalf("verdict=%s reason=%q", rep.Verdict, rep.AbortReason)
	}
	if s.faultCount(t) != 0 || rep.Injections[0].RemovedBy != "rollback" {
		t.Fatal("interrupt did not roll back")
	}
}

func TestInterruptBeforeInjection(t *testing.T) {
	s := newStack(t, engine.Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	rep := s.run(t, ctx, fmt.Sprintf(header, "early")+`
method:
  - proxy: pricing
    duration: 1s
    faults:
      - type: reset
`)
	if rep.Verdict != report.Errored || len(rep.Injections) != 0 {
		t.Fatalf("verdict=%s", rep.Verdict)
	}
}

func TestRollbackActionsAndStaggeredSteps(t *testing.T) {
	s := newStack(t, engine.Options{})
	var hooks atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hooks.Add(1)
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer hook.Close()
	s.env["HOOK"] = hook.URL
	// An extra fault installed outside the experiment is swept by the
	// clear_faults: "*" rollback action.
	if _, err := s.eng.AddFault("pricing", fault.Spec{ID: "stray", Type: fault.Latency, Latency: fault.Duration(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	rep := s.run(t, t.Context(), fmt.Sprintf(header, "rollback")+`
method:
  - name: first
    proxy: pricing
    duration: 300ms
    faults:
      - type: latency
        latency: 5ms
  - name: second
    proxy: edge
    at: 300ms
    duration: 300ms
    faults:
      - type: delay
        latency: 5ms
rollback:
  - clear_faults: "*"
  - clear_faults: pricing
  - http:
      url: ${HOOK}/ok
  - http:
      method: DELETE
      url: ${HOOK}/fail
  - http:
      url: http://127.0.0.1:1/unreachable
`)
	if hooks.Load() != 2 {
		t.Fatalf("hooks called %d times", hooks.Load())
	}
	if rep.Verdict != report.Errored || len(rep.Errors) != 2 {
		t.Fatalf("failing hooks must surface as errors: %s %v", rep.Verdict, rep.Errors)
	}
	if s.faultCount(t) != 0 {
		t.Fatal("stray fault not cleared")
	}
	if rep.Injections[1].InjectedAt.Sub(rep.Injections[0].InjectedAt) < 250*time.Millisecond {
		t.Fatal("step offsets not honoured")
	}
}

func TestAbortWindowExtendsToMinSamples(t *testing.T) {
	base := time.Unix(100, 0)
	var ss []probe.Sample
	for i := range 6 {
		ss = append(ss, probe.Sample{At: base.Add(time.Duration(i) * time.Second)})
	}
	if got := abortWindow(ss, base.Add(3*time.Second), 2); len(got) != 3 || !got[0].At.Equal(base.Add(3*time.Second)) {
		t.Fatalf("time window: %d samples", len(got))
	}
	if got := abortWindow(ss, base.Add(5*time.Second), 4); len(got) != 4 || !got[0].At.Equal(base.Add(2*time.Second)) {
		t.Fatalf("extended window: %d samples", len(got))
	}
	if got := abortWindow(ss[:2], base.Add(10*time.Second), 4); len(got) != 2 {
		t.Fatalf("short history: %d samples", len(got))
	}
}

func TestAttributedPhase(t *testing.T) {
	for _, c := range []struct{ start, end, want string }{
		{PhaseBefore, PhaseBefore, PhaseBefore},
		{PhaseBefore, PhaseMethod, PhaseMethod}, // sent before injection, answered during it
		{PhaseMethod, PhaseAfter, PhaseMethod},
		{PhaseRecovery, PhaseAfter, PhaseRecovery},
	} {
		if got := attributed(probe.Sample{Phase: c.start, EndPhase: c.end}); got != c.want {
			t.Errorf("%s->%s attributed to %s", c.start, c.end, got)
		}
	}
}
