// Package probe implements the steady-state probes an experiment uses to
// observe a system (HTTP requests and TCP request/response exchanges), and
// the recorder that collects their samples over time.
package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SlateGitOrg/faultline/internal/fault"
)

// DefaultTimeout bounds a probe when its spec sets none.
const DefaultTimeout = 2 * time.Second

// Sample is one probe observation.
type Sample struct {
	Probe string `json:"probe"`
	Phase string `json:"phase"`
	// EndPhase is the phase current when the probe completed; it differs
	// from Phase for probes that straddled a phase boundary.
	EndPhase string        `json:"end_phase,omitempty"`
	At       time.Time     `json:"at"`
	Latency  time.Duration `json:"latency_ns"`
	OK       bool          `json:"ok"`
	Err      string        `json:"err,omitempty"`
}

// HTTPSpec probes an HTTP endpoint.
type HTTPSpec struct {
	URL          string            `json:"url"`
	Method       string            `json:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	ExpectStatus []int             `json:"expect_status,omitempty"`
	BodyContains string            `json:"body_contains,omitempty"`
}

// TCPSpec probes a TCP service: connect, optionally send a payload and
// optionally require a substring in the first line of the reply.
type TCPSpec struct {
	Address string `json:"address"`
	Send    string `json:"send,omitempty"`
	Expect  string `json:"expect,omitempty"`
}

// Spec declares one probe. Exactly one of HTTP and TCP must be set.
type Spec struct {
	Name    string         `json:"name"`
	Timeout fault.Duration `json:"timeout,omitempty"`
	HTTP    *HTTPSpec      `json:"http,omitempty"`
	TCP     *TCPSpec       `json:"tcp,omitempty"`
}

// Validate checks the spec and applies defaults.
func (s *Spec) Validate() error {
	if s.Name == "" {
		return errors.New("probe needs a name")
	}
	if (s.HTTP == nil) == (s.TCP == nil) {
		return fmt.Errorf("probe %q must set exactly one of http or tcp", s.Name)
	}
	if s.Timeout < 0 {
		return fmt.Errorf("probe %q: timeout must not be negative", s.Name)
	}
	if s.Timeout == 0 {
		s.Timeout = fault.Duration(DefaultTimeout)
	}
	if h := s.HTTP; h != nil {
		if !strings.HasPrefix(h.URL, "http://") && !strings.HasPrefix(h.URL, "https://") {
			return fmt.Errorf("probe %q: url must be http(s), got %q", s.Name, h.URL)
		}
		if h.Method == "" {
			h.Method = http.MethodGet
		}
		h.Method = strings.ToUpper(h.Method)
		for _, c := range h.ExpectStatus {
			if c < 100 || c > 599 {
				return fmt.Errorf("probe %q: invalid expect_status %d", s.Name, c)
			}
		}
	}
	if t := s.TCP; t != nil {
		if _, _, err := net.SplitHostPort(t.Address); err != nil {
			return fmt.Errorf("probe %q: address must be host:port: %w", s.Name, err)
		}
	}
	return nil
}

// Prober executes a probe once.
type Prober interface {
	Name() string
	Probe(ctx context.Context) Sample
}

// New builds a prober from a validated spec.
func New(s Spec) Prober {
	if s.HTTP != nil {
		return &httpProber{spec: s, client: &http.Client{
			// A fresh connection per probe: keep-alive reuse and the
			// transport's silent retries would hide connection-level faults.
			Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}}
	}
	return &tcpProber{spec: s}
}

type httpProber struct {
	spec   Spec
	client *http.Client
}

func (p *httpProber) Name() string { return p.spec.Name }

func (p *httpProber) Probe(ctx context.Context) Sample {
	h := p.spec.HTTP
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout.D())
	defer cancel()
	start := time.Now()
	s := Sample{Probe: p.spec.Name, At: start}
	var body io.Reader
	if h.Body != "" {
		body = strings.NewReader(h.Body)
	}
	req, err := http.NewRequestWithContext(ctx, h.Method, h.URL, body)
	if err != nil {
		s.Err = "invalid request"
		return s
	}
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		s.Latency = time.Since(start)
		s.Err = Classify(err)
		return s
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	s.Latency = time.Since(start)
	if err != nil {
		s.Err = Classify(err)
		return s
	}
	if !statusOK(resp.StatusCode, h.ExpectStatus) {
		s.Err = fmt.Sprintf("status %d", resp.StatusCode)
		return s
	}
	if h.BodyContains != "" && !strings.Contains(string(data), h.BodyContains) {
		s.Err = "body mismatch"
		return s
	}
	s.OK = true
	return s
}

func statusOK(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 300
	}
	for _, c := range expect {
		if c == code {
			return true
		}
	}
	return false
}

type tcpProber struct{ spec Spec }

func (p *tcpProber) Name() string { return p.spec.Name }

func (p *tcpProber) Probe(ctx context.Context) Sample {
	t := p.spec.TCP
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout.D())
	defer cancel()
	start := time.Now()
	s := Sample{Probe: p.spec.Name, At: start}
	finish := func(err error) Sample {
		s.Latency = time.Since(start)
		if err != nil {
			s.Err = Classify(err)
		} else {
			s.OK = true
		}
		return s
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.Address)
	if err != nil {
		return finish(err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if t.Send != "" {
		if _, err := io.WriteString(conn, t.Send); err != nil {
			return finish(err)
		}
	}
	if t.Expect == "" {
		return finish(nil)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return finish(err)
	}
	if !strings.Contains(line, t.Expect) {
		s.Latency = time.Since(start)
		s.Err = "reply mismatch"
		return s
	}
	return finish(nil)
}

// Classify reduces an error to a short, stable label for reports.
func Classify(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection reset"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected EOF"
	case errors.Is(err, io.EOF):
		return "EOF"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}

// Recorder collects samples from concurrently running probes and labels
// each with the phase that was current when the probe *started*. Labelling
// by start time matters: a probe that was sent while a fault was active
// and completes just after rollback belongs to the fault phase, not to the
// recovery phase it happens to finish in.
type Recorder struct {
	mu      sync.Mutex
	marks   []phaseMark
	samples []Sample
}

type phaseMark struct {
	at   time.Time
	name string
}

// SetPhase starts phase p now.
func (r *Recorder) SetPhase(p string) {
	r.mu.Lock()
	r.marks = append(r.marks, phaseMark{at: time.Now(), name: p})
	r.mu.Unlock()
}

// Add records a sample under the phase current at its start time.
func (r *Recorder) Add(s Sample) {
	r.mu.Lock()
	s.Phase, s.EndPhase = "", ""
	if n := len(r.marks); n > 0 {
		s.EndPhase = r.marks[n-1].name
	}
	for i := len(r.marks) - 1; i >= 0; i-- {
		if !s.At.Before(r.marks[i].at) {
			s.Phase = r.marks[i].name
			break
		}
	}
	r.samples = append(r.samples, s)
	r.mu.Unlock()
}

// All returns a copy of every sample.
func (r *Recorder) All() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Sample(nil), r.samples...)
}

// Select returns samples for which keep returns true.
func (r *Recorder) Select(keep func(Sample) bool) []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Sample
	for _, s := range r.samples {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// Run executes every prober every interval until ctx ends, recording the
// samples. Each prober runs in its own goroutine; a prober that is slower
// than the interval skips ticks rather than piling up requests.
func Run(ctx context.Context, probers []Prober, interval time.Duration, rec *Recorder) {
	var wg sync.WaitGroup
	for _, p := range probers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				s := p.Probe(ctx)
				if ctx.Err() != nil {
					return // the run ended mid-probe; the sample is not meaningful
				}
				rec.Add(s)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	wg.Wait()
}
