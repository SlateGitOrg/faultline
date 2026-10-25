package experiment

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestExpandEnv(t *testing.T) {
	got, err := ExpandEnv("a=${A} b=${B:-dflt} c=${C:-}", env(map[string]string{"A": "1"}))
	if err != nil || got != "a=1 b=dflt c=" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ExpandEnv("${X} ${Y} ${X}", env(nil)); err == nil || !strings.Contains(err.Error(), "X, X, Y") {
		t.Fatalf("missing vars: %v", err)
	}
	if got, _ := ExpandEnv("$HOME and $$ stay", env(nil)); got != "$HOME and $$ stay" {
		t.Fatalf("bare $ must be left alone: %q", got)
	}
}

const valid = `
name: demo
description: test
control: ${CTL}
proxies:
  - name: edge
    kind: http
    upstream: http://127.0.0.1:9
  - name: db
    kind: tcp
    upstream: 127.0.0.1:9
probes:
  - name: api
    http:
      url: proxy://edge/products
      expect_status: [200]
  - name: raw
    tcp:
      address: proxy://db
      send: "PING\n"
      expect: PONG
steady_state:
  duration: 1s
  objectives:
    - probe: api
      min_success_rate: 0.99
      max_p95: 200ms
method:
  - proxy: db
    duration: 2s
    faults:
      - type: latency
        latency: 50ms
  - name: edge-errors
    proxy: edge
    at: 1s
    duration: 3s
    faults:
      - type: status
        status: 503
        probability: 0.1
abort:
  objectives:
    - probe: api
      min_success_rate: 0.5
rollback:
  - clear_faults: "*"
  - http:
      url: http://127.0.0.1:9/reset
`

func TestParseValidAndDefaults(t *testing.T) {
	s, err := Parse([]byte(valid), env(map[string]string{"CTL": "127.0.0.1:7070"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.Control != "127.0.0.1:7070" || s.Method[0].Name != "step-1" || s.Method[1].Name != "edge-errors" {
		t.Fatalf("parsed %+v", s)
	}
	if s.SteadyState.Interval.D() != DefaultInterval || s.Abort.Window.D() != DefaultAbortWindow ||
		s.Abort.MinSamples != DefaultMinSamples || s.Abort.CheckEvery != s.SteadyState.Interval ||
		s.Safety.FaultTTLGrace.D() != DefaultFaultTTLGrace || s.Safety.MaxDuration.D() != DefaultMaxDuration {
		t.Fatalf("defaults %+v %+v %+v", s.SteadyState, s.Abort, s.Safety)
	}
	if s.Rollback[1].HTTP.Method != http.MethodPost {
		t.Fatal("hook method default")
	}
	if s.MethodDuration() != 4*time.Second {
		t.Fatalf("method duration %v", s.MethodDuration())
	}
	if s.Probes[0].Timeout == 0 {
		t.Fatal("probe timeout default not propagated")
	}
	// The proxy:// URL is kept for the runner to resolve.
	if s.Probes[0].HTTP.URL != "proxy://edge/products" {
		t.Fatalf("url rewritten at parse time: %s", s.Probes[0].HTTP.URL)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.yaml")
	t.Setenv("CTL", "127.0.0.1:1")
	if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file")
	}
}

func TestParseErrors(t *testing.T) {
	base := map[string]string{"CTL": "x"}
	cases := []struct {
		name, doc, want string
	}{
		{"empty", "", "empty"},
		{"yaml", "name: [", "parse experiment"},
		{"unknown field", "name: x\ncontrl: y\n", "unknown field"},
		{"undefined env", "name: ${NOPE}", "NOPE"},
		{"not object", "- 1\n- 2\n", "parse experiment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc), env(base))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestValidateErrors(t *testing.T) {
	doc := `
proxies:
  - name: dup
    kind: tcp
  - name: dup
    kind: udp
  - kind: tcp
probes:
  - name: p
    http: {url: "proxy://ghost/x"}
  - name: p
    tcp: {address: nope}
  - name: q
    http: {url: "ftp://x"}
steady_state:
  duration: -1s
method:
  - proxy: ""
    duration: 0s
    at: -1s
  - proxy: dup
    duration: 1s
    faults:
      - type: status
        status: 500
      - type: latency
        latency: 1ms
        ttl: 1s
abort:
  window: -1s
  objectives:
    - probe: missing
      max_p95: 1s
    - probe: p
rollback:
  - {}
  - clear_faults: nowhere
  - http: {url: "ftp://x"}
safety:
  fault_ttl_grace: -1s
  max_duration: 500ms
`
	_, err := Parse([]byte(doc), env(nil))
	if err == nil {
		t.Fatal("invalid experiment accepted")
	}
	for _, want := range []string{
		"name is required", "control", "duplicate proxy", "kind \"udp\"", "every proxy needs a name",
		"undeclared proxy \"ghost\"", "duplicate probe", "address must be host:port", "url must be http(s)",
		"durations must not be negative", "at least one objective", "proxy is required", "duration must be positive",
		"at must not be negative", "at least one fault", "cannot be applied to a tcp proxy", "ttl is derived",
		"unknown probe \"missing\"", "sets no threshold", "exactly one of clear_faults or http",
		"unknown proxy \"nowhere\"", "http.url must be http(s)", "safety: durations", "longer than max_duration",
		"abort: window",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error %q in:\n%v", want, err)
		}
	}
	if _, err := Parse([]byte("name: x\ncontrol: y\nprobes: []\nsteady_state: {objectives: []}\n"), env(nil)); err == nil ||
		!strings.Contains(err.Error(), "at least one probe") || !strings.Contains(err.Error(), "at least one fault step") {
		t.Fatalf("empty lists: %v", err)
	}
}

func TestResolveAndDialable(t *testing.T) {
	cases := map[string]string{
		"[::]:80":        "127.0.0.1:80",
		":80":            "127.0.0.1:80",
		"0.0.0.0:80":     "127.0.0.1:80",
		"127.0.0.2:80":   "127.0.0.2:80",
		"not-an-address": "not-an-address",
	}
	for in, want := range cases {
		if got := dialable(in); got != want {
			t.Fatalf("dialable(%q)=%q", in, got)
		}
	}
}
