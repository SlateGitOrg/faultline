// Package experiment loads chaos experiment definitions and runs them:
// verify the steady-state hypothesis, inject the fault schedule while
// watching abort conditions, roll back, verify recovery and report.
package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/archit2501/faultline/internal/engine"
	"github.com/archit2501/faultline/internal/fault"
	"github.com/archit2501/faultline/internal/probe"
	"github.com/archit2501/faultline/internal/slo"
)

// Defaults applied by Validate.
const (
	DefaultSteadyDuration = 3 * time.Second
	DefaultInterval       = 200 * time.Millisecond
	DefaultAbortWindow    = 2 * time.Second
	DefaultMinSamples     = 5
	DefaultFaultTTLGrace  = 10 * time.Second
	DefaultMaxDuration    = 10 * time.Minute
)

// ProxyScheme lets probe targets refer to a proxy by name; the runner
// substitutes the proxy's actual listen address (proxy://edge/path).
const ProxyScheme = "proxy://"

// Spec is a complete experiment definition.
type Spec struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Control     string             `json:"control"`
	Proxies     []engine.ProxySpec `json:"proxies,omitempty"`
	Probes      []probe.Spec       `json:"probes"`
	SteadyState SteadyState        `json:"steady_state"`
	Method      []Step             `json:"method"`
	Abort       Abort              `json:"abort,omitempty"`
	Rollback    []RollbackAction   `json:"rollback,omitempty"`
	Safety      Safety             `json:"safety,omitempty"`
}

// SteadyState is the hypothesis: objectives that must hold before the
// faults, are expected to hold during them, and must hold again after.
type SteadyState struct {
	Duration      fault.Duration  `json:"duration,omitempty"`
	Interval      fault.Duration  `json:"interval,omitempty"`
	RecoveryGrace fault.Duration  `json:"recovery_grace,omitempty"`
	Objectives    []slo.Objective `json:"objectives"`
}

// Step is one entry of the fault schedule.
type Step struct {
	Name     string         `json:"name"`
	Proxy    string         `json:"proxy"`
	At       fault.Duration `json:"at,omitempty"`
	Duration fault.Duration `json:"duration"`
	Faults   []fault.Spec   `json:"faults"`
}

// Abort holds the conditions that stop the experiment early. They are
// evaluated continuously over a sliding window while faults are active.
type Abort struct {
	Window     fault.Duration  `json:"window,omitempty"`
	MinSamples int             `json:"min_samples,omitempty"`
	CheckEvery fault.Duration  `json:"check_every,omitempty"`
	Objectives []slo.Objective `json:"objectives,omitempty"`
}

// HTTPHook is a request issued as a rollback action.
type HTTPHook struct {
	Method string `json:"method,omitempty"`
	URL    string `json:"url"`
}

// RollbackAction is one extra rollback step. The runner always removes the
// faults it installed; these actions run afterwards.
type RollbackAction struct {
	ClearFaults string    `json:"clear_faults,omitempty"` // proxy name or "*"
	HTTP        *HTTPHook `json:"http,omitempty"`
}

// Safety bounds the experiment's blast radius in time.
type Safety struct {
	// FaultTTLGrace is added to every step's duration to form the fault's
	// server-side TTL, so faults expire even if the runner dies.
	FaultTTLGrace fault.Duration `json:"fault_ttl_grace,omitempty"`
	// MaxDuration is a hard cap on the fault-injection phase.
	MaxDuration fault.Duration `json:"max_duration,omitempty"`
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv substitutes ${VAR} and ${VAR:-default} using lookup. Unset
// variables without a default are an error, never silently empty.
func ExpandEnv(src string, lookup func(string) (string, bool)) (string, error) {
	var missing []string
	out := envRE.ReplaceAllStringFunc(src, func(m string) string {
		sub := envRE.FindStringSubmatch(m)
		if v, ok := lookup(sub[1]); ok && v != "" {
			return v
		}
		if sub[2] != "" {
			return sub[3]
		}
		missing = append(missing, sub[1])
		return m
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("undefined environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Load reads, expands and validates an experiment file.
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path) //nolint:gosec // reading the user's experiment file is the point
	if err != nil {
		return nil, err
	}
	return Parse(b, os.LookupEnv)
}

// Parse decodes YAML (or JSON) into a validated spec. Unknown fields are
// rejected so typos fail loudly instead of silently dropping a guard rail.
func Parse(src []byte, lookup func(string) (string, bool)) (*Spec, error) {
	expanded, err := ExpandEnv(string(src), lookup)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal([]byte(expanded), &doc); err != nil {
		return nil, fmt.Errorf("parse experiment: %w", err)
	}
	if doc == nil {
		return nil, errors.New("experiment file is empty")
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("parse experiment: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse experiment: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks internal consistency and fills in defaults.
func (s *Spec) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if s.Name == "" {
		bad("name is required")
	}
	if s.Control == "" {
		bad("control (the faultline control API address) is required")
	}

	kinds := map[string]fault.Kind{}
	for _, p := range s.Proxies {
		if p.Name == "" {
			bad("proxies: every proxy needs a name")
			continue
		}
		if _, dup := kinds[p.Name]; dup {
			bad("proxies: duplicate proxy %q", p.Name)
		}
		if p.Kind != fault.KindTCP && p.Kind != fault.KindHTTP {
			bad("proxies: %q has kind %q, want tcp or http", p.Name, p.Kind)
		}
		if _, dup := kinds[p.Name]; !dup {
			kinds[p.Name] = p.Kind
		}
	}

	probes := map[string]bool{}
	if len(s.Probes) == 0 {
		bad("probes: at least one probe is required")
	}
	for i := range s.Probes {
		p := &s.Probes[i]
		if probes[p.Name] {
			bad("probes: duplicate probe %q", p.Name)
		}
		probes[p.Name] = true
		// Validate a copy with proxy:// references replaced by a
		// placeholder address; the runner resolves the real one.
		cp := *p
		if cp.HTTP != nil {
			h := *cp.HTTP
			cp.HTTP = &h
		}
		if cp.TCP != nil {
			t := *cp.TCP
			cp.TCP = &t
		}
		if err := resolveProbe(&cp, func(name string) (string, error) {
			if _, ok := kinds[name]; !ok {
				return "", fmt.Errorf("probe %q refers to undeclared proxy %q", p.Name, name)
			}
			return "127.0.0.1:1", nil
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := cp.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("probes: %w", err))
			continue
		}
		p.Timeout = cp.Timeout
	}

	ss := &s.SteadyState
	if ss.Duration == 0 {
		ss.Duration = fault.Duration(DefaultSteadyDuration)
	}
	if ss.Interval == 0 {
		ss.Interval = fault.Duration(DefaultInterval)
	}
	if ss.Duration < 0 || ss.Interval < 0 || ss.RecoveryGrace < 0 {
		bad("steady_state: durations must not be negative")
	}
	if len(ss.Objectives) == 0 {
		bad("steady_state: at least one objective is required (that is the hypothesis)")
	}
	checkObjectives := func(where string, objs []slo.Objective) {
		for _, o := range objs {
			if err := o.Validate(); err != nil {
				bad("%s: %w", where, err)
			} else if !probes[o.Probe] {
				bad("%s: objective refers to unknown probe %q", where, o.Probe)
			}
		}
	}
	checkObjectives("steady_state", ss.Objectives)

	if len(s.Method) == 0 {
		bad("method: at least one fault step is required")
	}
	for i := range s.Method {
		st := &s.Method[i]
		if st.Name == "" {
			st.Name = fmt.Sprintf("step-%d", i+1)
		}
		if st.Proxy == "" {
			bad("method %q: proxy is required", st.Name)
		}
		if st.Duration <= 0 {
			bad("method %q: duration must be positive", st.Name)
		}
		if st.At < 0 {
			bad("method %q: at must not be negative", st.Name)
		}
		if len(st.Faults) == 0 {
			bad("method %q: at least one fault is required", st.Name)
		}
		if kind, ok := kinds[st.Proxy]; ok && (kind == fault.KindTCP || kind == fault.KindHTTP) {
			for _, f := range st.Faults {
				cp := f
				if err := cp.Validate(kind); err != nil {
					bad("method %q: %w", st.Name, err)
				}
			}
		}
		for _, f := range st.Faults {
			if f.TTL != 0 {
				bad("method %q: fault ttl is derived from the step duration; remove it", st.Name)
				break
			}
		}
	}

	ab := &s.Abort
	if ab.Window == 0 {
		ab.Window = fault.Duration(DefaultAbortWindow)
	}
	if ab.MinSamples == 0 {
		ab.MinSamples = DefaultMinSamples
	}
	if ab.CheckEvery == 0 {
		ab.CheckEvery = ss.Interval
	}
	if ab.Window < 0 || ab.CheckEvery < 0 || ab.MinSamples < 0 {
		bad("abort: window, check_every and min_samples must not be negative")
	}
	checkObjectives("abort", ab.Objectives)

	for i, rb := range s.Rollback {
		switch {
		case rb.ClearFaults != "" && rb.HTTP == nil:
			if _, ok := kinds[rb.ClearFaults]; !ok && rb.ClearFaults != "*" && !stepProxy(s.Method, rb.ClearFaults) {
				bad("rollback[%d]: clear_faults refers to unknown proxy %q", i, rb.ClearFaults)
			}
		case rb.HTTP != nil && rb.ClearFaults == "":
			if !strings.HasPrefix(rb.HTTP.URL, "http://") && !strings.HasPrefix(rb.HTTP.URL, "https://") {
				bad("rollback[%d]: http.url must be http(s)", i)
			}
			if rb.HTTP.Method == "" {
				rb.HTTP.Method = "POST"
			}
		default:
			bad("rollback[%d]: set exactly one of clear_faults or http", i)
		}
	}

	sf := &s.Safety
	if sf.FaultTTLGrace == 0 {
		sf.FaultTTLGrace = fault.Duration(DefaultFaultTTLGrace)
	}
	if sf.MaxDuration == 0 {
		sf.MaxDuration = fault.Duration(DefaultMaxDuration)
	}
	if sf.FaultTTLGrace < 0 || sf.MaxDuration < 0 {
		bad("safety: durations must not be negative")
	}
	if end := s.MethodDuration(); end > sf.MaxDuration.D() {
		bad("safety: the fault schedule lasts %s, longer than max_duration %s", end, sf.MaxDuration)
	}
	return errors.Join(errs...)
}

func stepProxy(steps []Step, name string) bool {
	for _, st := range steps {
		if st.Proxy == name {
			return true
		}
	}
	return false
}

// MethodDuration is the length of the fault schedule.
func (s *Spec) MethodDuration() time.Duration {
	var end time.Duration
	for _, st := range s.Method {
		end = max(end, st.At.D()+st.Duration.D())
	}
	return end
}

// resolveProbe replaces proxy:// references in a probe with real addresses.
func resolveProbe(p *probe.Spec, addr func(name string) (string, error)) error {
	if h := p.HTTP; h != nil && strings.HasPrefix(h.URL, ProxyScheme) {
		rest := strings.TrimPrefix(h.URL, ProxyScheme)
		name, path, _ := strings.Cut(rest, "/")
		a, err := addr(name)
		if err != nil {
			return err
		}
		h.URL = "http://" + a + "/" + path
	}
	if t := p.TCP; t != nil && strings.HasPrefix(t.Address, ProxyScheme) {
		a, err := addr(strings.TrimPrefix(t.Address, ProxyScheme))
		if err != nil {
			return err
		}
		t.Address = a
	}
	return nil
}

// dialable turns a listen address such as "[::]:8080" or ":8080" into one
// a client can connect to.
func dialable(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
