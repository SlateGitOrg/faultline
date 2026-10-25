package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SlateGitOrg/faultline/internal/engine"
	"github.com/SlateGitOrg/faultline/internal/fault"
	"github.com/SlateGitOrg/faultline/internal/sample"
)

type result struct {
	code           int
	stdout, stderr string
}

func invoke(t *testing.T, vars map[string]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	e := &env{stdout: &out, stderr: &errb, getenv: func(k string) string { return vars[k] }}
	code := mainEnv(t.Context(), e, args)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// daemon starts `faultline serve` in process and returns the environment
// client commands need to reach it.
func daemon(t *testing.T, extra ...string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "addr")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() {
		e := &env{stdout: &bytes.Buffer{}, stderr: &errb, getenv: func(string) string { return "" }}
		args := append([]string{"serve", "--listen", "127.0.0.1:0", "--addr-file", addrFile, "--seed", "3", "--log-level", "warn"}, extra...)
		done <- mainEnv(ctx, e, args)
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("serve exited %d: %s", code, errb.String())
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(addrFile)
		if err == nil && len(b) > 0 {
			return map[string]string{EnvControl: strings.TrimSpace(string(b))}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not start: %s", errb.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUsage(t *testing.T) {
	if r := invoke(t, nil); r.code != exitUsage || !strings.Contains(r.stdout, "Usage: faultline") {
		t.Fatalf("%+v", r)
	}
	if r := invoke(t, nil, "help"); r.code != 0 || !strings.Contains(r.stdout, "killswitch") {
		t.Fatalf("%+v", r)
	}
	if r := invoke(t, nil, "bogus"); r.code != exitUsage || !strings.Contains(r.stderr, `unknown command "bogus"`) {
		t.Fatalf("%+v", r)
	}
	if r := invoke(t, nil, "version"); r.code != 0 || !strings.HasPrefix(r.stdout, "faultline ") {
		t.Fatalf("%+v", r)
	}
	if r := invoke(t, nil, "fault", "add", "-h"); r.code != 0 || !strings.Contains(r.stderr, "-latency") {
		t.Fatalf("help: %+v", r)
	}
	for _, args := range [][]string{
		{"proxy"}, {"proxy", "zap"}, {"fault"}, {"fault", "zap"},
		{"fault", "add"}, {"fault", "add", "p"}, {"fault", "add", "p", "--type", "latency", "--json", "{}"},
		{"fault", "add", "p", "--json", "{\"nope\":1}"}, {"fault", "add", "p", "--header", "novalue"},
		{"fault", "rm", "p"}, {"fault", "clear"}, {"fault", "clear", "p", "--all"},
		{"proxy", "create", "x"}, {"proxy", "rm"}, {"killswitch"}, {"killswitch", "maybe"},
		{"run"}, {"validate"}, {"serve", "--log-level", "loud"}, {"serve", "--log-format", "xml"},
		{"serve", "--allow-upstream", "not-an-ip"}, {"health", "--nope"},
	} {
		if r := invoke(t, nil, args...); r.code != exitUsage {
			t.Errorf("%v: code %d, stderr %s", args, r.code, r.stderr)
		}
	}
}

func TestServeRefusesUnauthenticatedPublicListen(t *testing.T) {
	r := invoke(t, nil, "serve", "--listen", "0.0.0.0:0")
	if r.code != exitUsage || !strings.Contains(r.stderr, EnvToken) {
		t.Fatalf("%+v", r)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true,
		":1": false, "0.0.0.0:1": false, "10.0.0.1:1": false, "garbage": false,
	} {
		if got := isLoopbackListen(addr); got != want {
			t.Errorf("isLoopbackListen(%q)=%v", addr, got)
		}
	}
}

func TestServeTokenAndListenErrors(t *testing.T) {
	// With a token the daemon may bind every interface; the client must
	// then present it.
	ctx, cancel := context.WithCancel(t.Context())
	addrFile := filepath.Join(t.TempDir(), "addr")
	done := make(chan int, 1)
	go func() {
		e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, getenv: func(k string) string {
			if k == EnvToken {
				return "s3cret"
			}
			return ""
		}}
		done <- mainEnv(ctx, e, []string{"serve", "--listen", "127.0.0.1:0", "--addr-file", addrFile, "--log-format", "json"})
	}()
	var addr string
	for range 500 {
		if b, err := os.ReadFile(addrFile); err == nil && len(b) > 0 {
			addr = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := invoke(t, map[string]string{EnvControl: addr}, "proxy", "ls"); r.code != exitError || !strings.Contains(r.stderr, "401") {
		t.Fatalf("no token: %+v", r)
	}
	if r := invoke(t, map[string]string{EnvControl: addr, EnvToken: "s3cret"}, "proxy", "ls"); r.code != 0 {
		t.Fatalf("token: %+v", r)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exit %d", code)
	}
	// Address already in use, and an unwritable addr file.
	vars := daemon(t)
	if r := invoke(t, nil, "serve", "--listen", vars[EnvControl]); r.code != exitError {
		t.Fatalf("port clash: %+v", r)
	}
	if r := invoke(t, nil, "serve", "--listen", "127.0.0.1:0", "--addr-file", filepath.Join(t.TempDir(), "missing", "addr")); r.code != exitError {
		t.Fatalf("addr file: %+v", r)
	}
}

func TestAllowPublicListen(t *testing.T) {
	vars := daemon(t, "--allow-public-listen")
	pricing, err := sample.StartPricing("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pricing.Close()
	r := invoke(t, vars, "proxy", "create", "public", "--kind", "tcp", "--listen", "0.0.0.0:0", "--upstream", pricing.Addr())
	if r.code != 0 || !strings.Contains(r.stdout, "listening on") {
		t.Fatalf("--allow-public-listen: %+v", r)
	}
}

func TestProxyAndFaultLifecycle(t *testing.T) {
	vars := daemon(t, "--max-ttl", "1m")
	pricing, err := sample.StartPricing("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pricing.Close()

	if r := invoke(t, vars, "health", "--wait", "1s"); r.code != 0 || !strings.Contains(r.stdout, "ok (version") {
		t.Fatalf("health: %+v", r)
	}
	r := invoke(t, vars, "proxy", "create", "pricing", "--kind", "tcp", "--upstream", pricing.Addr())
	if r.code != 0 || !strings.Contains(r.stdout, "proxy pricing (tcp) listening on 127.0.0.1:") {
		t.Fatalf("create: %+v", r)
	}
	if r := invoke(t, vars, "proxy", "create", "edge", "--kind", "http", "--upstream", "http://"+pricing.Addr()); r.code != 0 {
		t.Fatalf("create http: %+v", r)
	}
	if r := invoke(t, vars, "proxy", "create", "evil", "--upstream", "203.0.113.7:80"); r.code != exitError || !strings.Contains(r.stderr, "allowlist") {
		t.Fatalf("allowlist: %+v", r)
	}
	r = invoke(t, vars, "fault", "add", "pricing", "--id", "slow", "--type", "latency", "--latency", "50ms", "--jitter", "5ms",
		"--direction", "both", "--probability", "0.5", "--ttl", "30s", "--max-affected", "3", "--source", "127.0.0.1, ::1")
	if r.code != 0 || !strings.Contains(r.stdout, "fault slow on pricing: latency 50ms±5ms both p=0.5") {
		t.Fatalf("add: %+v", r)
	}
	r = invoke(t, vars, "fault", "add", "edge", "--type", "status", "--status", "503", "--body", "down",
		"--header", "Retry-After=1", "--match-method", "get,post", "--match-path", "/api", "--match-header", "X-Canary=1")
	if r.code != 0 || !strings.Contains(r.stdout, "status 503 scoped") {
		t.Fatalf("add http: %+v", r)
	}
	if r := invoke(t, vars, "fault", "add", "edge", "--json", `{"id":"brk","type":"abort","probability":0.1}`); r.code != 0 {
		t.Fatalf("add json: %+v", r)
	}
	// An explicit --probability 0 is kept (registered, never applied), not
	// widened to the default 1; an explicit --rate 0 is rejected.
	if r := invoke(t, vars, "fault", "add", "pricing", "--id", "off", "--type", "reset", "--probability", "0"); r.code != 0 || !strings.Contains(r.stdout, "fault off on pricing: reset p=0") {
		t.Fatalf("probability 0: %+v", r)
	}
	if r := invoke(t, vars, "fault", "rm", "pricing", "off"); r.code != 0 {
		t.Fatalf("rm off: %+v", r)
	}
	if r := invoke(t, vars, "fault", "add", "edge", "--type", "corrupt", "--rate", "0"); r.code != exitError || !strings.Contains(r.stderr, "corrupt rate") {
		t.Fatalf("rate 0: %+v", r)
	}
	if r := invoke(t, vars, "fault", "add", "edge", "--type", "abort", "--probability", "half"); r.code != exitUsage {
		t.Fatalf("bad probability flag: %+v", r)
	}
	if r := invoke(t, vars, "proxy", "create", "public", "--kind", "tcp", "--listen", "0.0.0.0:0", "--upstream", pricing.Addr()); r.code != exitError || !strings.Contains(r.stderr, "--allow-public-listen") {
		t.Fatalf("public listen must be refused by default: %+v", r)
	}
	if r := invoke(t, vars, "fault", "add", "pricing", "--type", "latency", "--ttl", "2h", "--latency", "1ms"); r.code != exitError || !strings.Contains(r.stderr, "403") {
		t.Fatalf("ttl policy: %+v", r)
	}

	r = invoke(t, vars, "proxy", "ls")
	if r.code != 0 || !strings.Contains(r.stdout, "NAME") || !strings.Contains(r.stdout, "pricing") || !strings.Contains(r.stdout, "0 reqs") {
		t.Fatalf("ls: %+v", r)
	}
	r = invoke(t, vars, "proxy", "ls", "--json")
	var ps []engine.ProxyInfo
	if err := json.Unmarshal([]byte(r.stdout), &ps); err != nil || len(ps) != 2 {
		t.Fatalf("ls json: %v %s", err, r.stdout)
	}
	r = invoke(t, vars, "fault", "ls", "pricing")
	if r.code != 0 || !strings.Contains(r.stdout, "slow") || !strings.Contains(r.stdout, "in 30s") {
		t.Fatalf("fault ls: %+v", r)
	}
	r = invoke(t, vars, "fault", "ls", "edge", "--json")
	var fis []fault.Info
	if err := json.Unmarshal([]byte(r.stdout), &fis); err != nil || len(fis) != 2 || fis[0].Match.Headers["X-Canary"] != "1" {
		t.Fatalf("fault ls json: %v %s", err, r.stdout)
	}
	if r := invoke(t, vars, "fault", "rm", "pricing", "slow"); r.code != 0 {
		t.Fatalf("rm: %+v", r)
	}
	if r := invoke(t, vars, "fault", "rm", "pricing", "slow"); r.code != exitError || !strings.Contains(r.stderr, "404") {
		t.Fatalf("rm twice: %+v", r)
	}
	if r := invoke(t, vars, "fault", "clear", "edge"); r.code != 0 || !strings.Contains(r.stdout, "2 fault(s) removed") {
		t.Fatalf("clear: %+v", r)
	}
	if r := invoke(t, vars, "fault", "add", "edge", "--type", "delay", "--latency", "1ms"); r.code != 0 {
		t.Fatalf("re-add: %+v", r)
	}

	// Kill switch: removes everything and refuses new faults.
	if r := invoke(t, vars, "killswitch", "on", "--reason", "pager"); r.code != 0 || !strings.Contains(r.stdout, "ENGAGED: pager") {
		t.Fatalf("kill on: %+v", r)
	}
	if r := invoke(t, vars, "killswitch", "status"); !strings.Contains(r.stdout, "engaged since") {
		t.Fatalf("kill status: %+v", r)
	}
	if r := invoke(t, vars, "fault", "add", "edge", "--type", "abort"); r.code != exitError || !strings.Contains(r.stderr, "423") {
		t.Fatalf("add under kill switch: %+v", r)
	}
	if r := invoke(t, vars, "killswitch", "on"); r.code != 0 {
		t.Fatalf("kill on default reason: %+v", r)
	}
	if r := invoke(t, vars, "killswitch", "off"); r.code != 0 {
		t.Fatalf("kill off: %+v", r)
	}
	if r := invoke(t, vars, "killswitch", "status"); r.stdout != "released\n" {
		t.Fatalf("kill status: %+v", r)
	}
	if r := invoke(t, vars, "fault", "clear", "--all"); r.code != 0 || !strings.Contains(r.stdout, "0 fault(s)") {
		t.Fatalf("clear all: %+v", r)
	}

	r = invoke(t, vars, "audit")
	for _, want := range []string{"proxy.create", "proxy.reject", "fault.add", "fault.reject", "killswitch.engage", "killswitch.release"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("audit lacks %s:\n%s", want, r.stdout)
		}
	}
	r = invoke(t, vars, "audit", "--since", "3", "--json")
	var evs []engine.Event
	if err := json.Unmarshal([]byte(r.stdout), &evs); err != nil || len(evs) == 0 || evs[0].Seq != 4 {
		t.Fatalf("audit json: %v %s", err, r.stdout)
	}
	if r := invoke(t, vars, "proxy", "rm", "edge"); r.code != 0 {
		t.Fatalf("proxy rm: %+v", r)
	}
	if r := invoke(t, vars, "proxy", "rm", "edge"); r.code != exitError {
		t.Fatalf("proxy rm twice: %+v", r)
	}
}

func TestClientCommandsWithoutDaemon(t *testing.T) {
	vars := map[string]string{EnvControl: "127.0.0.1:1"}
	for _, args := range [][]string{
		{"health"}, {"proxy", "ls"}, {"fault", "ls", "x"}, {"audit"}, {"killswitch", "status"},
		{"killswitch", "off"}, {"killswitch", "on"}, {"fault", "clear", "x"}, {"proxy", "create", "x", "--upstream", "127.0.0.1:2"},
		{"fault", "add", "x", "--type", "abort"},
	} {
		if r := invoke(t, vars, args...); r.code != exitError {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

const experimentDoc = `
name: cli-%s
control: %s
proxies:
  - name: pricing
    kind: tcp
    upstream: %s
probes:
  - name: ping
    timeout: 500ms
    tcp:
      address: proxy://pricing
      send: "PING\n"
      expect: PONG
steady_state:
  duration: 300ms
  interval: 25ms
  objectives:
    - probe: ping
      min_success_rate: 0.9
      max_p95: %s
method:
  - proxy: pricing
    duration: 300ms
    faults:
      - type: latency
        latency: 40ms
`

func TestRunAndValidate(t *testing.T) {
	vars := daemon(t)
	pricing, err := sample.StartPricing("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pricing.Close()
	dir := t.TempDir()
	write := func(name, bound string) string {
		p := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(p, fmt.Appendf(nil, experimentDoc, name, vars[EnvControl], pricing.Addr(), bound), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pass := write("pass", "200ms")
	fail := write("fail", "20ms")
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := invoke(t, nil, "validate", pass, fail)
	if r.code != 0 || strings.Count(r.stdout, "ok   ") != 2 || !strings.Contains(r.stdout, "schedule 300ms") {
		t.Fatalf("validate: %+v", r)
	}
	r = invoke(t, nil, "validate", pass, bad)
	if r.code != exitError || !strings.Contains(r.stdout, "FAIL "+bad) || !strings.Contains(r.stderr, "1 of 2") {
		t.Fatalf("validate bad: %+v", r)
	}

	out := filepath.Join(dir, "reports")
	r = invoke(t, nil, "run", pass, "--out", out)
	if r.code != 0 || !strings.Contains(r.stdout, "# Experiment `cli-pass`: PASSED") || !strings.Contains(r.stderr, "verdict: PASSED (exit 0)") {
		t.Fatalf("run pass: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(out, "report.json")); err != nil {
		t.Fatal(err)
	}
	// 40ms of injected latency breaks a 20ms p95 objective: verdict failed, exit 2.
	r = invoke(t, nil, "run", fail, "--out", out, "--json")
	if r.code != 2 || !strings.Contains(r.stdout, `"verdict": "failed"`) {
		t.Fatalf("run fail: %+v", r)
	}
	r = invoke(t, nil, "run", pass, "--out", out, "--quiet", "--control", vars[EnvControl])
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("run quiet: %+v", r)
	}
	if r := invoke(t, nil, "run", bad); r.code != exitError || !strings.Contains(r.stderr, "bad.yaml") {
		t.Fatalf("run bad: %+v", r)
	}
	if r := invoke(t, nil, "run", pass, "--log-level", "x"); r.code != exitUsage {
		t.Fatalf("run log level: %+v", r)
	}
	// An unwritable report directory is an error after the run.
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, nil, "run", pass, "--quiet", "--out", filepath.Join(blocker, "sub")); r.code != exitError || !strings.Contains(r.stderr, "write report") {
		t.Fatalf("run unwritable: %+v", r)
	}
	// Default output directory is relative to the working directory.
	t.Chdir(dir)
	if r := invoke(t, nil, "run", pass, "--quiet"); r.code != 0 {
		t.Fatalf("run default out: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "faultline-reports", "cli-pass", "report.md")); err != nil {
		t.Fatal(err)
	}
}

func TestKVFlag(t *testing.T) {
	k := kvFlag{}
	if err := k.Set("b=2"); err != nil {
		t.Fatal(err)
	}
	if err := k.Set("a=1=x"); err != nil {
		t.Fatal(err)
	}
	if k.String() != "a=1=x,b=2" || k.Set("=v") == nil {
		t.Fatalf("%q", k.String())
	}
	var d fault.Duration
	df := durationFlag{&d}
	if df.Set("nope") == nil || df.Set("15ms") != nil || df.String() != "15ms" || (durationFlag{}).String() != "0s" {
		t.Fatal("duration flag")
	}
	if got := splitList(" a, ,b,"); len(got) != 2 || got[1] != "b" {
		t.Fatalf("%q", got)
	}
}
