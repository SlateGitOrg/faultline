package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/fault"
)

func mustProber(t *testing.T, s Spec) Prober {
	t.Helper()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	return New(s)
}

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		case "/redirect":
			http.Redirect(w, r, "/", http.StatusFound)
			return
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			_, _ = fmt.Fprintf(w, "%s %s %s", r.Method, r.Header.Get("X-Probe"), b)
			return
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer srv.Close()

	cases := []struct {
		name string
		spec HTTPSpec
		to   time.Duration
		ok   bool
		err  string
	}{
		{"ok", HTTPSpec{URL: srv.URL + "/"}, 0, true, ""},
		{"body", HTTPSpec{URL: srv.URL + "/", BodyContains: "healthy"}, 0, true, ""},
		{"body mismatch", HTTPSpec{URL: srv.URL + "/", BodyContains: "sick"}, 0, false, "body mismatch"},
		{"status", HTTPSpec{URL: srv.URL + "/teapot"}, 0, false, "status 418"},
		{"expected status", HTTPSpec{URL: srv.URL + "/teapot", ExpectStatus: []int{418}}, 0, true, ""},
		{"redirect not followed", HTTPSpec{URL: srv.URL + "/redirect"}, 0, false, "status 302"},
		{"timeout", HTTPSpec{URL: srv.URL + "/slow"}, 50 * time.Millisecond, false, "timeout"},
		{"echo", HTTPSpec{URL: srv.URL + "/echo", Method: "post", Body: "hi", Headers: map[string]string{"X-Probe": "1"}, BodyContains: "POST 1 hi"}, 0, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := c.spec
			p := mustProber(t, Spec{Name: c.name, HTTP: &spec, Timeout: fault.Duration(c.to)})
			s := p.Probe(t.Context())
			if s.OK != c.ok || s.Err != c.err || s.Probe != c.name || p.Name() != c.name {
				t.Fatalf("got ok=%v err=%q", s.OK, s.Err)
			}
			if s.Latency <= 0 {
				t.Fatal("latency not measured")
			}
		})
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	_ = ln.Close()
	s := mustProber(t, Spec{Name: "refused", HTTP: &HTTPSpec{URL: "http://" + dead}}).Probe(t.Context())
	if s.OK || s.Err != "connection refused" {
		t.Fatalf("refused: %+v", s)
	}
	bad := New(Spec{Name: "bad", Timeout: fault.Duration(time.Second), HTTP: &HTTPSpec{URL: "http://x", Method: "BAD METHOD"}})
	if s := bad.Probe(t.Context()); s.Err != "invalid request" {
		t.Fatalf("invalid request: %+v", s)
	}
}

func startLineServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "PING":
					_, _ = io.WriteString(c, "PONG\n")
				case "HANG":
					time.Sleep(time.Second)
				default:
					_, _ = io.WriteString(c, "ERR\n")
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTCPProbe(t *testing.T) {
	addr := startLineServer(t)
	cases := []struct {
		name string
		spec TCPSpec
		ok   bool
		err  string
	}{
		{"connect only", TCPSpec{Address: addr}, true, ""},
		{"ping", TCPSpec{Address: addr, Send: "PING\n", Expect: "PONG"}, true, ""},
		{"mismatch", TCPSpec{Address: addr, Send: "WHAT\n", Expect: "PONG"}, false, "reply mismatch"},
		{"hang", TCPSpec{Address: addr, Send: "HANG\n", Expect: "PONG"}, false, "timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := c.spec
			s := mustProber(t, Spec{Name: c.name, TCP: &spec, Timeout: fault.Duration(200 * time.Millisecond)}).Probe(t.Context())
			if s.OK != c.ok || s.Err != c.err {
				t.Fatalf("got ok=%v err=%q", s.OK, s.Err)
			}
		})
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	_ = ln.Close()
	if s := mustProber(t, Spec{Name: "dead", TCP: &TCPSpec{Address: dead}}).Probe(t.Context()); s.Err != "connection refused" {
		t.Fatalf("dead: %+v", s)
	}
	// A server that closes without replying yields EOF.
	eofLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer eofLn.Close()
	go func() {
		for {
			c, err := eofLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	s := mustProber(t, Spec{Name: "eof", TCP: &TCPSpec{Address: eofLn.Addr().String(), Expect: "x"}}).Probe(t.Context())
	if s.OK || (s.Err != "EOF" && s.Err != "connection reset") {
		t.Fatalf("eof: %+v", s)
	}
	if mustProber(t, Spec{Name: "n", TCP: &TCPSpec{Address: addr}}).Name() != "n" {
		t.Fatal("name")
	}
}

func TestValidate(t *testing.T) {
	bad := []Spec{
		{},
		{Name: "none"},
		{Name: "both", HTTP: &HTTPSpec{URL: "http://x"}, TCP: &TCPSpec{Address: "x:1"}},
		{Name: "neg", Timeout: -1, TCP: &TCPSpec{Address: "x:1"}},
		{Name: "scheme", HTTP: &HTTPSpec{URL: "ftp://x"}},
		{Name: "status", HTTP: &HTTPSpec{URL: "http://x", ExpectStatus: []int{42}}},
		{Name: "addr", TCP: &TCPSpec{Address: "nope"}},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("%q accepted", s.Name)
		}
	}
	s := Spec{Name: "d", HTTP: &HTTPSpec{URL: "https://x"}}
	if err := s.Validate(); err != nil || s.Timeout.D() != DefaultTimeout || s.HTTP.Method != "GET" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "t" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	cases := map[string]error{
		"timeout":            context.DeadlineExceeded,
		"connection refused": &net.OpError{Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		"connection reset":   fmt.Errorf("x: %w", syscall.ECONNRESET),
		"unexpected EOF":     io.ErrUnexpectedEOF,
		"EOF":                io.EOF,
		"canceled":           context.Canceled,
		"error":              errors.New("other"),
	}
	for want, err := range cases {
		if got := Classify(err); got != want {
			t.Fatalf("%v: got %q want %q", err, got, want)
		}
	}
	if Classify(timeoutErr{}) != "timeout" {
		t.Fatal("net timeout")
	}
}

type countProber struct{ n int }

func (c *countProber) Name() string { return "count" }
func (c *countProber) Probe(context.Context) Sample {
	c.n++
	return Sample{Probe: "count", At: time.Now(), OK: c.n%2 == 0}
}

func TestRunAndRecorder(t *testing.T) {
	rec := &Recorder{}
	rec.SetPhase("warmup")
	ctx, cancel := context.WithTimeout(t.Context(), 220*time.Millisecond)
	defer cancel()
	cp := &countProber{}
	go func() {
		time.Sleep(110 * time.Millisecond)
		rec.SetPhase("main")
	}()
	Run(ctx, []Prober{cp}, 20*time.Millisecond, rec)
	all := rec.All()
	if len(all) < 6 || len(all) > 13 {
		t.Fatalf("expected ~11 samples at 20ms over 220ms, got %d", len(all))
	}
	warm := rec.Select(func(s Sample) bool { return s.Phase == "warmup" })
	main := rec.Select(func(s Sample) bool { return s.Phase == "main" })
	if len(warm) == 0 || len(main) == 0 || len(warm)+len(main) != len(all) {
		t.Fatalf("phases: warm=%d main=%d all=%d", len(warm), len(main), len(all))
	}
}

func TestRecorderLabelsByStartTime(t *testing.T) {
	rec := &Recorder{}
	early := time.Now().Add(-time.Hour)
	rec.Add(Sample{Probe: "p", At: early})
	rec.SetPhase("fault")
	inFlight := time.Now()
	rec.SetPhase("recovery")
	rec.Add(Sample{Probe: "p", At: inFlight}) // finished after the phase changed
	rec.Add(Sample{Probe: "p", At: time.Now()})
	got := []string{}
	for _, s := range rec.All() {
		got = append(got, s.Phase)
	}
	if strings.Join(got, ",") != ",fault,recovery" {
		t.Fatalf("phases %q", got)
	}
}
