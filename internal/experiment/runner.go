package experiment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/archit2501/faultline/internal/engine"
	"github.com/archit2501/faultline/internal/fault"
	"github.com/archit2501/faultline/internal/probe"
	"github.com/archit2501/faultline/internal/report"
	"github.com/archit2501/faultline/internal/slo"
)

// Phase names used in reports.
const (
	PhaseBefore   = "steady-state-before"
	PhaseMethod   = "method"
	PhaseRecovery = "recovery-grace"
	PhaseAfter    = "steady-state-after"
)

// rollbackTimeout bounds cleanup. Rollback runs on its own context so that
// cancelling the experiment (Ctrl-C, abort) never skips it.
const rollbackTimeout = 15 * time.Second

// Controller is the subset of the control API the runner needs;
// *control.Client implements it.
type Controller interface {
	Proxy(ctx context.Context, name string) (engine.ProxyInfo, error)
	CreateProxy(ctx context.Context, spec engine.ProxySpec) (engine.ProxyInfo, error)
	DeleteProxy(ctx context.Context, name string) error
	AddFault(ctx context.Context, proxy string, spec fault.Spec) (fault.Info, error)
	RemoveFault(ctx context.Context, proxy, id string) error
	ClearFaults(ctx context.Context, proxy string) (int, error)
	ClearAll(ctx context.Context) (int, error)
}

// Runner executes experiments against a control API.
type Runner struct {
	Control Controller
	Logger  *slog.Logger
	// HTTP is used for rollback hooks.
	HTTP *http.Client
	// IsNotFound recognises "already gone" errors from Control, which
	// rollback treats as success (the fault expired or was removed).
	IsNotFound func(error) bool
}

// run holds the mutable state of one execution.
type run struct {
	r    *Runner
	spec *Spec
	rep  *report.Report
	rec  *probe.Recorder
	log  *slog.Logger

	mu         sync.Mutex
	injections []*report.Injection
	created    []string
}

func (x *run) event(kind, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	x.mu.Lock()
	x.rep.Timeline = append(x.rep.Timeline, report.Event{At: time.Now(), Kind: kind, Message: msg})
	x.mu.Unlock()
	x.log.Info(msg, "event", kind)
}

func (x *run) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	x.mu.Lock()
	x.rep.Errors = append(x.rep.Errors, msg)
	x.mu.Unlock()
	x.log.Error(msg)
}

// Run executes spec and returns its report. Every outcome, including setup
// errors, aborts and interruptions, is expressed in the report's verdict.
func (r *Runner) Run(ctx context.Context, spec *Spec) *report.Report {
	log := r.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	x := &run{
		r: r, spec: spec, rec: &probe.Recorder{},
		log: log.With("experiment", spec.Name),
		rep: &report.Report{
			Experiment: spec.Name, Description: spec.Description,
			StartedAt: time.Now(), Hypothesis: "not evaluated",
			Injections: []report.Injection{}, Timeline: []report.Event{},
		},
	}
	x.execute(ctx)
	x.rep.EndedAt = time.Now()
	x.finishBuckets()
	return x.rep
}

func (x *run) execute(ctx context.Context) {
	spec := x.spec
	x.event("start", "experiment %q started", spec.Name)
	probers, err := x.setup(ctx)
	defer x.teardown()
	if err != nil {
		x.fail("setup: %v", err)
		x.rep.Verdict = report.Errored
		return
	}

	probeCtx, stopProbes := context.WithCancel(context.Background())
	probesDone := make(chan struct{})
	go func() {
		defer close(probesDone)
		probe.Run(probeCtx, probers, spec.SteadyState.Interval.D(), x.rec)
	}()
	stop := sync.OnceFunc(func() { stopProbes(); <-probesDone })
	defer stop()

	// 1. Steady state must hold before anything is injected.
	before, ok := x.observe(ctx, PhaseBefore, spec.SteadyState.Duration.D())
	if !ok {
		x.rep.Verdict = report.Errored
		x.fail("interrupted before fault injection")
		return
	}
	if !slo.AllPass(before.Checks) {
		x.event("precondition", "steady state did not hold before injection; no faults injected")
		x.rep.Verdict = report.PreconditionFailed
		return
	}
	x.event("precondition", "steady state verified")

	// 2. Fault schedule with continuous abort monitoring.
	method := x.method(ctx)

	// 3. Rollback always runs, on its own context.
	x.rollback()
	if ctx.Err() != nil {
		// Interrupted: faults are rolled back; skip the recovery window.
		x.rep.Verdict = report.Aborted
		return
	}

	// 4. Recovery.
	if g := spec.SteadyState.RecoveryGrace.D(); g > 0 {
		if _, ok := x.observe(context.Background(), PhaseRecovery, g); !ok {
			return
		}
	}
	x.observe(context.Background(), PhaseAfter, spec.SteadyState.Duration.D())
	// Probes still in flight when a phase ended belong to that phase (they
	// are labelled by start time), so stop probing and re-summarise every
	// phase before judging it.
	stop()
	x.resummarize()
	method = x.phase(PhaseMethod)
	recovered := slo.AllPass(x.phase(PhaseAfter).Checks)
	x.rep.Recovered = &recovered
	if recovered {
		x.event("recovery", "steady state restored after rollback")
	} else {
		x.event("recovery", "steady state NOT restored after rollback")
	}

	held := slo.AllPass(method.Checks)
	x.rep.Hypothesis = "held"
	if !held {
		x.rep.Hypothesis = "deviated"
	}
	switch {
	case x.rep.AbortReason != "":
		x.rep.Verdict = report.Aborted
	case len(x.rep.Errors) > 0:
		x.rep.Verdict = report.Errored
	case !held || !recovered:
		x.rep.Verdict = report.Failed
	default:
		x.rep.Verdict = report.Passed
	}
	x.event("verdict", "verdict: %s (hypothesis %s, recovered %v)", x.rep.Verdict, x.rep.Hypothesis, recovered)
}

// setup ensures the declared proxies exist, checks the proxies used by the
// schedule, and builds probers with proxy:// references resolved.
func (x *run) setup(ctx context.Context) ([]probe.Prober, error) {
	addrs := map[string]string{}
	for _, ps := range x.spec.Proxies {
		info, err := x.r.Control.Proxy(ctx, ps.Name)
		switch {
		case err == nil:
			if info.Kind != ps.Kind {
				return nil, fmt.Errorf("proxy %q exists with kind %s, experiment declares %s", ps.Name, info.Kind, ps.Kind)
			}
			x.event("setup", "using existing proxy %s (%s %s -> %s)", ps.Name, info.Kind, info.Listen, info.Upstream)
		case x.notFound(err):
			info, err = x.r.Control.CreateProxy(ctx, ps)
			if err != nil {
				return nil, fmt.Errorf("create proxy %q: %w", ps.Name, err)
			}
			x.created = append(x.created, ps.Name)
			x.event("setup", "created proxy %s (%s %s -> %s)", ps.Name, info.Kind, info.Listen, info.Upstream)
		default:
			return nil, fmt.Errorf("look up proxy %q: %w", ps.Name, err)
		}
		addrs[ps.Name] = dialable(info.Listen)
	}
	for _, st := range x.spec.Method {
		if _, ok := addrs[st.Proxy]; ok {
			continue
		}
		info, err := x.r.Control.Proxy(ctx, st.Proxy)
		if err != nil {
			return nil, fmt.Errorf("method step %q targets proxy %q: %w", st.Name, st.Proxy, err)
		}
		addrs[st.Proxy] = dialable(info.Listen)
	}
	probers := make([]probe.Prober, 0, len(x.spec.Probes))
	for _, ps := range x.spec.Probes {
		cp := ps
		if cp.HTTP != nil {
			h := *cp.HTTP
			cp.HTTP = &h
		}
		if cp.TCP != nil {
			t := *cp.TCP
			cp.TCP = &t
		}
		if err := resolveProbe(&cp, func(name string) (string, error) {
			a, ok := addrs[name]
			if !ok {
				return "", fmt.Errorf("probe %q refers to unknown proxy %q", ps.Name, name)
			}
			return a, nil
		}); err != nil {
			return nil, err
		}
		if err := cp.Validate(); err != nil {
			return nil, err
		}
		probers = append(probers, probe.New(cp))
	}
	return probers, nil
}

func (x *run) notFound(err error) bool {
	return x.r.IsNotFound != nil && x.r.IsNotFound(err)
}

// observe labels samples with phase for d and evaluates the steady-state
// objectives over them. It reports false if ctx ended first.
func (x *run) observe(ctx context.Context, phase string, d time.Duration) (report.Phase, bool) {
	start := time.Now()
	x.rec.SetPhase(phase)
	t := time.NewTimer(d)
	defer t.Stop()
	ok := true
	select {
	case <-t.C:
	case <-ctx.Done():
		ok = false
	}
	ph := x.summarize(phase, start, time.Now())
	x.rep.Phases = append(x.rep.Phases, ph)
	return ph, ok
}

func (x *run) summarize(phase string, start, end time.Time) report.Phase {
	ph := report.Phase{Name: phase, Start: start, End: end}
	for _, p := range x.spec.Probes {
		samples := x.rec.Select(func(s probe.Sample) bool { return s.Phase == phase && s.Probe == p.Name })
		ph.Probes = append(ph.Probes, report.ProbeStats{Probe: p.Name, Stats: slo.Summarize(samples)})
	}
	if phase != PhaseRecovery {
		for _, o := range x.spec.SteadyState.Objectives {
			ph.Checks = append(ph.Checks, slo.Evaluate(o, statsFor(ph, o.Probe))...)
		}
	}
	return ph
}

// resummarize recomputes every recorded phase from the full sample set.
func (x *run) resummarize() {
	for i, ph := range x.rep.Phases {
		x.rep.Phases[i] = x.summarize(ph.Name, ph.Start, ph.End)
	}
}

// phase returns the recorded summary of the named phase.
func (x *run) phase(name string) report.Phase {
	for _, ph := range x.rep.Phases {
		if ph.Name == name {
			return ph
		}
	}
	return report.Phase{Name: name}
}

func statsFor(ph report.Phase, probe string) slo.Stats {
	for _, ps := range ph.Probes {
		if ps.Probe == probe {
			return ps.Stats
		}
	}
	return slo.Stats{}
}

// method runs the fault schedule. It returns the method phase summary.
func (x *run) method(parent context.Context) report.Phase {
	spec := x.spec
	ctx, cancel := context.WithTimeout(parent, spec.Safety.MaxDuration.D())
	defer cancel()
	start := time.Now()
	x.rec.SetPhase(PhaseMethod)
	x.event("method", "fault schedule started (%d steps, %s)", len(spec.Method), spec.MethodDuration())

	var abortOnce sync.Once
	abort := func(reason string, checks []slo.Check) {
		abortOnce.Do(func() {
			x.mu.Lock()
			x.rep.AbortReason = reason
			x.rep.AbortChecks = checks
			x.mu.Unlock()
			x.event("abort", "ABORT: %s", reason)
			cancel()
		})
	}

	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		x.monitor(ctx, start, abort)
	}()

	var wg sync.WaitGroup
	for _, st := range spec.Method {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x.runStep(ctx, start, st, abort)
		}()
	}
	wg.Wait()
	switch {
	case ctx.Err() == nil:
		x.event("method", "fault schedule completed")
	case parent.Err() != nil:
		abort("interrupted", nil)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		abort(fmt.Sprintf("safety.max_duration %s exceeded", spec.Safety.MaxDuration), nil)
	}
	cancel()
	<-monitorDone
	end := time.Now()
	ph := x.summarize(PhaseMethod, start, end)
	x.rep.Phases = append(x.rep.Phases, ph)
	return ph
}

// monitor evaluates abort objectives over a sliding window until ctx ends.
func (x *run) monitor(ctx context.Context, start time.Time, abort func(string, []slo.Check)) {
	ab := x.spec.Abort
	if len(ab.Objectives) == 0 {
		return
	}
	t := time.NewTicker(ab.CheckEvery.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			from := now.Add(-ab.Window.D())
			if from.Before(start) {
				from = start
			}
			var checks []slo.Check
			breached := false
			for _, o := range ab.Objectives {
				samples := abortWindow(x.rec.Select(func(s probe.Sample) bool {
					return s.Phase == PhaseMethod && s.Probe == o.Probe
				}), from, ab.MinSamples)
				if len(samples) < ab.MinSamples {
					continue // not enough evidence in the window yet
				}
				cs := slo.Evaluate(o, slo.Summarize(samples))
				checks = append(checks, cs...)
				if !slo.AllPass(cs) {
					breached = true
				}
			}
			if breached {
				var failed []string
				for _, c := range checks {
					if !c.Pass {
						failed = append(failed, fmt.Sprintf("%s %s %s (got %s)", c.Probe, c.Metric, c.Want, c.Got))
					}
				}
				abort(fmt.Sprintf("abort condition breached over the last %s: %v", ab.Window, failed), checks)
				return
			}
		}
	}
}

// abortWindow selects the samples an abort check evaluates: those taken at
// or after from, extended backwards to the most recent minSamples samples
// when the time window holds fewer. Without the extension a severe outage
// starves its own abort check: probes that time out take longer than the
// probe interval, so a short window can never collect minSamples failures.
// method holds one probe's method-phase samples in recording order, which
// is also time order because each prober runs sequentially.
func abortWindow(method []probe.Sample, from time.Time, minSamples int) []probe.Sample {
	i := len(method)
	for i > 0 && !method[i-1].At.Before(from) {
		i--
	}
	if n := len(method) - i; n < minSamples {
		i = max(0, len(method)-minSamples)
	}
	return method[i:]
}

// runStep waits for the step's start offset, installs its faults with a
// server-side TTL, waits for its duration and removes them.
func (x *run) runStep(ctx context.Context, start time.Time, st Step, abort func(string, []slo.Check)) {
	if !sleepCtx(ctx, time.Until(start.Add(st.At.D()))) {
		return
	}
	ttl := st.Duration.D() + x.spec.Safety.FaultTTLGrace.D()
	var mine []*report.Injection
	for _, f := range st.Faults {
		if ctx.Err() != nil {
			return
		}
		f.TTL = fault.Duration(ttl)
		in := &report.Injection{Step: st.Name, Proxy: st.Proxy, Type: string(f.Type), Detail: f.Describe(), InjectedAt: time.Now()}
		// The call itself is not cancelled by an abort: a fault the server
		// installs must be known to the runner so rollback removes it.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		info, err := x.r.Control.AddFault(callCtx, st.Proxy, f)
		cancel()
		x.mu.Lock()
		if err != nil {
			in.Error = err.Error()
		} else {
			in.ID = info.ID
		}
		x.injections = append(x.injections, in)
		x.mu.Unlock()
		if err != nil {
			x.fail("step %q: inject %s on %s: %v", st.Name, f.Type, st.Proxy, err)
			abort(fmt.Sprintf("fault injection failed in step %q", st.Name), nil)
			return
		}
		mine = append(mine, in)
		x.event("inject", "step %s: %s on %s (%s, ttl %s)", st.Name, info.ID, st.Proxy, in.Detail, ttl)
	}
	if !sleepCtx(ctx, st.Duration.D()) {
		return // rollback removes the faults
	}
	for _, in := range mine {
		x.removeInjection(context.Background(), in, "schedule")
	}
}

func (x *run) removeInjection(ctx context.Context, in *report.Injection, by string) {
	x.mu.Lock()
	done := in.RemovedAt != nil || in.ID == ""
	x.mu.Unlock()
	if done {
		return
	}
	err := x.r.Control.RemoveFault(ctx, in.Proxy, in.ID)
	if err != nil && !x.notFound(err) {
		x.fail("remove fault %s on %s: %v", in.ID, in.Proxy, err)
		return
	}
	now := time.Now()
	x.mu.Lock()
	in.RemovedAt = &now
	in.RemovedBy = by
	if err != nil {
		in.RemovedBy = by + ", already gone"
	}
	x.mu.Unlock()
	x.event("remove", "%s removed from %s (%s)", in.ID, in.Proxy, in.RemovedBy)
}

// rollback removes every fault this run installed and then runs the
// user-defined rollback actions.
func (x *run) rollback() {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	x.event("rollback", "rollback started")
	x.mu.Lock()
	ins := append([]*report.Injection(nil), x.injections...)
	x.mu.Unlock()
	for _, in := range ins {
		x.removeInjection(ctx, in, "rollback")
	}
	for _, rb := range x.spec.Rollback {
		switch {
		case rb.ClearFaults == "*":
			n, err := x.r.Control.ClearAll(ctx)
			if err != nil {
				x.fail("rollback clear_faults *: %v", err)
				continue
			}
			x.event("rollback", "cleared %d faults on all proxies", n)
		case rb.ClearFaults != "":
			n, err := x.r.Control.ClearFaults(ctx, rb.ClearFaults)
			if err != nil {
				x.fail("rollback clear_faults %s: %v", rb.ClearFaults, err)
				continue
			}
			x.event("rollback", "cleared %d faults on %s", n, rb.ClearFaults)
		case rb.HTTP != nil:
			x.hook(ctx, rb.HTTP)
		}
	}
	x.event("rollback", "rollback finished")
}

func (x *run) hook(ctx context.Context, h *HTTPHook) {
	client := x.r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, h.Method, h.URL, nil)
	if err != nil {
		x.fail("rollback hook %s %s: %v", h.Method, h.URL, err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		x.fail("rollback hook %s %s: %v", h.Method, h.URL, err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		x.fail("rollback hook %s %s: status %d", h.Method, h.URL, resp.StatusCode)
		return
	}
	x.event("rollback", "hook %s %s -> %d", h.Method, h.URL, resp.StatusCode)
}

// teardown deletes the proxies this run created and finalises injections.
func (x *run) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	for _, name := range x.created {
		if err := x.r.Control.DeleteProxy(ctx, name); err != nil && !x.notFound(err) {
			x.fail("teardown: delete proxy %s: %v", name, err)
			continue
		}
		x.event("teardown", "deleted proxy %s", name)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, in := range x.injections {
		x.rep.Injections = append(x.rep.Injections, *in)
	}
}

// finishBuckets builds the per-probe timeline over the whole run, choosing
// a bucket width that keeps the timeline at most 60 columns wide.
func (x *run) finishBuckets() {
	all := x.rec.All()
	if len(all) == 0 {
		return
	}
	total := x.rep.EndedAt.Sub(x.rep.StartedAt)
	width := 250 * time.Millisecond
	for total/width > 60 {
		width *= 2
	}
	x.rep.BucketWidth = width
	x.rep.Buckets = map[string][]slo.Bucket{}
	for _, p := range x.spec.Probes {
		samples := x.rec.Select(func(s probe.Sample) bool { return s.Probe == p.Name })
		x.rep.Buckets[p.Name] = slo.Buckets(samples, x.rep.StartedAt, x.rep.EndedAt, width)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
