// Package report defines the experiment report and renders it as JSON and
// Markdown.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SlateGitOrg/faultline/internal/slo"
)

// Verdict is the overall outcome of an experiment.
type Verdict string

// Verdicts, from best to worst.
const (
	// Passed: steady state held before, during and after the faults.
	Passed Verdict = "passed"
	// Failed: the hypothesis was falsified (SLOs deviated during the
	// method) or the system did not recover afterwards.
	Failed Verdict = "failed"
	// Aborted: an abort condition breached and the run was cut short.
	Aborted Verdict = "aborted"
	// PreconditionFailed: steady state did not hold before any fault was
	// injected, so nothing was injected.
	PreconditionFailed Verdict = "precondition_failed"
	// Errored: the run could not be carried out (setup or control errors).
	Errored Verdict = "error"
)

// ProbeStats are the statistics of one probe in one phase.
type ProbeStats struct {
	Probe string `json:"probe"`
	slo.Stats
}

// Phase is one stage of the run.
type Phase struct {
	Name   string       `json:"name"`
	Start  time.Time    `json:"start"`
	End    time.Time    `json:"end"`
	Probes []ProbeStats `json:"probes"`
	Checks []slo.Check  `json:"checks,omitempty"`
}

// Injection records one fault the runner installed.
type Injection struct {
	Step       string     `json:"step"`
	Proxy      string     `json:"proxy"`
	ID         string     `json:"id,omitempty"`
	Type       string     `json:"type"`
	Detail     string     `json:"detail"`
	InjectedAt time.Time  `json:"injected_at"`
	RemovedAt  *time.Time `json:"removed_at,omitempty"`
	RemovedBy  string     `json:"removed_by,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Event is one timeline entry.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// Report is the full result of one experiment run.
type Report struct {
	Experiment  string                  `json:"experiment"`
	Description string                  `json:"description,omitempty"`
	Verdict     Verdict                 `json:"verdict"`
	Hypothesis  string                  `json:"hypothesis"`
	Recovered   *bool                   `json:"recovered,omitempty"`
	AbortReason string                  `json:"abort_reason,omitempty"`
	AbortChecks []slo.Check             `json:"abort_checks,omitempty"`
	StartedAt   time.Time               `json:"started_at"`
	EndedAt     time.Time               `json:"ended_at"`
	Phases      []Phase                 `json:"phases"`
	Injections  []Injection             `json:"injections"`
	Timeline    []Event                 `json:"timeline"`
	BucketWidth time.Duration           `json:"bucket_width_ns"`
	Buckets     map[string][]slo.Bucket `json:"buckets,omitempty"`
	Errors      []string                `json:"errors,omitempty"`
}

// ExitCode maps a verdict to a process exit status for CI use.
func (v Verdict) ExitCode() int {
	switch v {
	case Passed:
		return 0
	case Failed:
		return 2
	case Aborted:
		return 3
	case PreconditionFailed:
		return 4
	default:
		return 1
	}
}

// WriteFiles writes report.json and report.md into dir.
func (r *Report) WriteFiles(dir string) (jsonPath, mdPath string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", err
	}
	jsonPath = filepath.Join(dir, "report.json")
	mdPath = filepath.Join(dir, "report.md")
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, append(b, '\n'), 0o600); err != nil {
		return "", "", err
	}
	var md strings.Builder
	r.Markdown(&md)
	if err := os.WriteFile(mdPath, []byte(md.String()), 0o600); err != nil {
		return "", "", err
	}
	return jsonPath, mdPath, nil
}

func (r *Report) offset(t time.Time) string {
	return "t+" + slo.Round(t.Sub(r.StartedAt)).String()
}

// Markdown renders the report for humans.
func (r *Report) Markdown(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("# Experiment `%s`: %s\n\n", r.Experiment, strings.ToUpper(string(r.Verdict)))
	if r.Description != "" {
		p("> %s\n\n", strings.ReplaceAll(strings.TrimSpace(r.Description), "\n", "\n> "))
	}
	p("| | |\n|---|---|\n")
	p("| Verdict | **%s** |\n", r.Verdict)
	p("| Hypothesis during faults | %s |\n", r.Hypothesis)
	if r.Recovered != nil {
		rec := "no"
		if *r.Recovered {
			rec = "yes"
		}
		p("| Recovered after rollback | %s |\n", rec)
	}
	if r.AbortReason != "" {
		p("| Abort reason | %s |\n", r.AbortReason)
	}
	p("| Started | %s |\n", r.StartedAt.UTC().Format(time.RFC3339))
	p("| Duration | %s |\n\n", slo.Round(r.EndedAt.Sub(r.StartedAt)))

	if len(r.Errors) > 0 {
		p("## Errors\n\n")
		for _, e := range r.Errors {
			p("- %s\n", e)
		}
		p("\n")
	}

	p("## Timeline\n\n| When | Event | Detail |\n|---|---|---|\n")
	for _, ev := range r.Timeline {
		p("| %s | %s | %s |\n", r.offset(ev.At), ev.Kind, escape(ev.Message))
	}
	p("\n")

	p("## Faults injected\n\n")
	if len(r.Injections) == 0 {
		p("None.\n\n")
	} else {
		p("| Step | Proxy | Fault | Spec | Injected | Removed |\n|---|---|---|---|---|---|\n")
		for _, in := range r.Injections {
			removed := "—"
			if in.RemovedAt != nil {
				removed = r.offset(*in.RemovedAt) + " (" + in.RemovedBy + ")"
			}
			id := in.ID
			if in.Error != "" {
				id = "failed: " + in.Error
			}
			p("| %s | %s | %s | %s | %s | %s |\n", in.Step, in.Proxy, escape(id), escape(in.Detail), r.offset(in.InjectedAt), removed)
		}
		p("\n")
	}

	if len(r.AbortChecks) > 0 {
		p("## Abort conditions at breach\n\n")
		writeChecks(w, r.AbortChecks)
	}

	p("## Phases\n\n")
	for _, ph := range r.Phases {
		p("### %s (%s → %s)\n\n", ph.Name, r.offset(ph.Start), r.offset(ph.End))
		p("| Probe | Samples | Success | p50 | p95 | p99 | max | Errors |\n|---|---|---|---|---|---|---|---|\n")
		for _, ps := range ph.Probes {
			p("| %s | %d | %.1f%% | %s | %s | %s | %s | %s |\n", ps.Probe, ps.Count, 100*ps.SuccessRate,
				slo.Round(ps.P50), slo.Round(ps.P95), slo.Round(ps.P99), slo.Round(ps.Max), errorSummary(ps.Errors))
		}
		p("\n")
		if len(ph.Checks) > 0 {
			writeChecks(w, ph.Checks)
		}
	}

	if len(r.Buckets) > 0 {
		p("## Probe timeline\n\n")
		p("One column per %s from the start of the run. Bar height is that slice's p95 latency relative to the slowest slice; `x` marks slices with failed probes.\n\n```\n", slo.Round(r.BucketWidth))
		names := make([]string, 0, len(r.Buckets))
		for n := range r.Buckets {
			names = append(names, n)
		}
		slices.Sort(names)
		width := 0
		for _, n := range names {
			width = max(width, len(n))
		}
		for _, n := range names {
			bars, errs, peak := Sparkline(r.Buckets[n])
			p("%-*s  %s  peak p95 %s\n", width, n, bars, slo.Round(peak))
			if errs != "" {
				p("%-*s  %s\n", width, "", errs)
			}
		}
		p("```\n")
	}
}

func writeChecks(w io.Writer, checks []slo.Check) {
	for _, c := range checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		_, _ = fmt.Fprintf(w, "- %s `%s` %s %s, got %s\n", mark, c.Probe, c.Metric, c.Want, c.Got)
	}
	_, _ = fmt.Fprintln(w)
}

func errorSummary(errs map[string]int) string {
	if len(errs) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if errs[a] != errs[b] {
			return errs[b] - errs[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s ×%d", k, errs[k])
	}
	return escape(strings.Join(parts, ", "))
}

func escape(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

var blocks = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders bucket p95 latencies as a bar line plus an error marker
// line, and returns the peak p95.
func Sparkline(bs []slo.Bucket) (bars, errs string, peak time.Duration) {
	for _, b := range bs {
		peak = max(peak, b.P95)
	}
	var bb, eb strings.Builder
	for _, b := range bs {
		switch {
		case b.Count == 0:
			bb.WriteRune(' ')
		case peak == 0:
			bb.WriteRune(blocks[0])
		default:
			i := int(float64(b.P95) / float64(peak) * float64(len(blocks)-1))
			bb.WriteRune(blocks[i])
		}
		if b.Errors > 0 {
			eb.WriteRune('x')
		} else {
			eb.WriteRune(' ')
		}
	}
	return bb.String(), strings.TrimRight(eb.String(), " "), peak
}
