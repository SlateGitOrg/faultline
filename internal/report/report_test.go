package report

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/slo"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixture() *Report {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	removed := at(2600)
	rec := true
	return &Report{
		Experiment:  "pricing-latency",
		Description: "Catalog tolerates\nslow pricing.",
		Verdict:     Aborted,
		Hypothesis:  "deviated",
		Recovered:   &rec,
		AbortReason: "abort condition breached",
		AbortChecks: []slo.Check{{Probe: "catalog", Metric: "success_rate", Want: ">= 0.900", Got: "0.400 (4/10)", Pass: false}},
		StartedAt:   t0,
		EndedAt:     at(5000),
		Phases: []Phase{{
			Name: "method", Start: at(1000), End: at(2600),
			Probes: []ProbeStats{{Probe: "catalog", Stats: slo.Stats{Count: 10, Successes: 4, SuccessRate: 0.4,
				P50: 250 * time.Millisecond, P95: 251 * time.Millisecond, P99: 251 * time.Millisecond, Max: 252 * time.Millisecond,
				Errors: map[string]int{"status 503": 5, "timeout": 1}}}},
			Checks: []slo.Check{{Probe: "catalog", Metric: "p95", Want: "<= 200ms", Got: "251ms", Pass: false}},
		}},
		Injections: []Injection{
			{Step: "slow", Proxy: "pricing", ID: "pricing-latency-1", Type: "latency", Detail: "latency 400ms downstream", InjectedAt: at(1000), RemovedAt: &removed, RemovedBy: "rollback"},
			{Step: "edge", Proxy: "edge", Type: "status", Detail: "status 503", InjectedAt: at(1000), Error: "403 | blocked"},
		},
		Timeline:    []Event{{At: at(0), Kind: "start", Message: "started"}, {At: at(2500), Kind: "abort", Message: "ABORT: a|b"}},
		BucketWidth: time.Second,
		Buckets: map[string][]slo.Bucket{
			"catalog": {{Count: 5, P95: 10 * time.Millisecond}, {Count: 5, Errors: 3, P95: 250 * time.Millisecond}, {}, {Count: 2, P95: 0}},
			"pricing": {{Count: 0}},
		},
		Errors: []string{"rollback hook failed"},
	}
}

func TestMarkdownGolden(t *testing.T) {
	var b strings.Builder
	fixture().Markdown(&b)
	golden := filepath.Join("testdata", "report.golden.md")
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("markdown differs from golden (run go test ./internal/report -update):\n%s", b.String())
	}
}

func TestWriteFiles(t *testing.T) {
	dir := t.TempDir()
	r := fixture()
	jp, mp, err := r.WriteFiles(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	b, _ := os.ReadFile(jp)
	if err := json.Unmarshal(b, &back); err != nil || back.Verdict != Aborted || len(back.Injections) != 2 {
		t.Fatalf("json round trip: %v %+v", err, back)
	}
	md, _ := os.ReadFile(mp)
	if !strings.Contains(string(md), "ABORTED") {
		t.Fatal("markdown file")
	}
	// A file where a directory is expected makes WriteFiles fail.
	blocker := filepath.Join(dir, "blocker")
	_ = os.WriteFile(blocker, nil, 0o600)
	if _, _, err := r.WriteFiles(blocker); err == nil {
		t.Fatal("expected error")
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[Verdict]int{Passed: 0, Failed: 2, Aborted: 3, PreconditionFailed: 4, Errored: 1}
	for v, want := range cases {
		if v.ExitCode() != want {
			t.Fatalf("%s -> %d", v, v.ExitCode())
		}
	}
}

func TestSparkline(t *testing.T) {
	bars, errs, peak := Sparkline([]slo.Bucket{{Count: 1, P95: 10}, {Count: 1, P95: 80, Errors: 1}, {}, {Count: 1}})
	if bars != "▁█ ▁" || errs != " x" || peak != 80 {
		t.Fatalf("%q %q %v", bars, errs, peak)
	}
	bars, _, _ = Sparkline([]slo.Bucket{{Count: 3}})
	if bars != "▁" {
		t.Fatalf("zero peak %q", bars)
	}
	r := fixture()
	r.Description, r.Recovered, r.AbortReason, r.Errors, r.Injections, r.AbortChecks, r.Buckets = "", nil, "", nil, nil, nil, nil
	var b strings.Builder
	r.Markdown(&b)
	if !strings.Contains(b.String(), "## Faults injected\n\nNone.") || strings.Contains(b.String(), "Probe timeline") {
		t.Fatalf("minimal report:\n%s", b.String())
	}
	rec := false
	r.Recovered = &rec
	b.Reset()
	r.Markdown(&b)
	if !strings.Contains(b.String(), "| Recovered after rollback | no |") {
		t.Fatal("recovered=no")
	}
}
