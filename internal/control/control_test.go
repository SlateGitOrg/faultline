package control

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/archit2501/faultline/internal/engine"
	"github.com/archit2501/faultline/internal/fault"
)

func setup(t *testing.T, token string) (*Client, string) {
	t.Helper()
	eng := engine.New(engine.Options{Seed: 1})
	t.Cleanup(func() { _ = eng.Close() })
	srv := httptest.NewServer(NewHandler(eng, HandlerOptions{Token: token}))
	t.Cleanup(srv.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return NewClient(srv.URL, token), ln.Addr().String()
}

func TestClientServerRoundTrip(t *testing.T) {
	c, up := setup(t, "")
	ctx := t.Context()
	h, err := c.Health(ctx)
	if err != nil || h.Status != "ok" || h.Proxies != 0 {
		t.Fatalf("health %+v %v", h, err)
	}
	pi, err := c.CreateProxy(ctx, engine.ProxySpec{Name: "db", Kind: fault.KindTCP, Upstream: up})
	if err != nil || pi.Listen == "" || pi.TCP == nil {
		t.Fatalf("create %+v %v", pi, err)
	}
	if _, err := c.CreateProxy(ctx, engine.ProxySpec{Name: "edge", Kind: fault.KindHTTP, Upstream: "http://" + up}); err != nil {
		t.Fatal(err)
	}
	fi, err := c.AddFault(ctx, "db", fault.Spec{Type: fault.Latency, Latency: fault.Duration(10 * time.Millisecond), TTL: fault.Duration(time.Minute)})
	if err != nil || fi.ID == "" || fi.ExpiresAt == nil {
		t.Fatalf("add fault %+v %v", fi, err)
	}
	faults, err := c.Faults(ctx, "db")
	if err != nil || len(faults) != 1 || faults[0].Latency.D() != 10*time.Millisecond {
		t.Fatalf("faults %+v %v", faults, err)
	}
	got, err := c.Proxy(ctx, "db")
	if err != nil || len(got.Faults) != 1 {
		t.Fatalf("proxy %+v %v", got, err)
	}
	if err := c.RemoveFault(ctx, "db", fi.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveFault(ctx, "db", fi.ID); !IsNotFound(err) {
		t.Fatalf("want 404, got %v", err)
	}
	_, _ = c.AddFault(ctx, "db", fault.Spec{Type: fault.Reset})
	_, _ = c.AddFault(ctx, "db", fault.Spec{Type: fault.Blackhole})
	if n, err := c.ClearFaults(ctx, "db"); err != nil || n != 2 {
		t.Fatalf("clear %d %v", n, err)
	}
	_, _ = c.AddFault(ctx, "edge", fault.Spec{Type: fault.Abort})
	if n, err := c.ClearAll(ctx); err != nil || n != 1 {
		t.Fatalf("clear all %d %v", n, err)
	}
	ks, err := c.SetKillSwitch(ctx, true, "")
	if err != nil || !ks.Engaged || ks.Reason == "" {
		t.Fatalf("kill %+v %v", ks, err)
	}
	_, err = c.AddFault(ctx, "db", fault.Spec{Type: fault.Reset})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusLocked {
		t.Fatalf("want 423 while killed, got %v", err)
	}
	if ks, _ := c.KillSwitch(ctx); !ks.Engaged {
		t.Fatal("kill state")
	}
	if _, err := c.SetKillSwitch(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	ps, err := c.Proxies(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("list %v %v", ps, err)
	}
	ev, err := c.Audit(ctx, 0)
	if err != nil || len(ev) == 0 {
		t.Fatalf("audit %v %v", ev, err)
	}
	last := ev[len(ev)-1].Seq
	if ev2, _ := c.Audit(ctx, last); len(ev2) != 0 {
		t.Fatal("audit since filter")
	}
	if err := c.DeleteProxy(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Proxy(ctx, "db"); !IsNotFound(err) {
		t.Fatal("deleted proxy still present")
	}
	if c.Base() == "" {
		t.Fatal("base")
	}
}

func TestErrorMapping(t *testing.T) {
	c, up := setup(t, "")
	ctx := t.Context()
	status := func(err error) int {
		var ae *APIError
		if errors.As(err, &ae) {
			return ae.Status
		}
		t.Fatalf("not an API error: %v", err)
		return 0
	}
	_, err := c.CreateProxy(ctx, engine.ProxySpec{Name: "x", Kind: fault.KindTCP, Upstream: "8.8.8.8:53"})
	if status(err) != http.StatusForbidden {
		t.Fatalf("blocked upstream: %v", err)
	}
	_, err = c.CreateProxy(ctx, engine.ProxySpec{Name: "x", Kind: "udp", Upstream: up})
	if status(err) != http.StatusBadRequest {
		t.Fatal(err)
	}
	_, _ = c.CreateProxy(ctx, engine.ProxySpec{Name: "x", Kind: fault.KindTCP, Upstream: up})
	_, err = c.CreateProxy(ctx, engine.ProxySpec{Name: "x", Kind: fault.KindTCP, Upstream: up})
	if status(err) != http.StatusConflict {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "409") {
		t.Fatalf("error text %q", err)
	}
	_, err = c.Faults(ctx, "nope")
	if !IsNotFound(err) {
		t.Fatal(err)
	}
	if _, err := c.ClearFaults(ctx, "nope"); !IsNotFound(err) {
		t.Fatal(err)
	}
	if err := c.DeleteProxy(ctx, "nope"); !IsNotFound(err) {
		t.Fatal(err)
	}
	if _, err := c.AddFault(ctx, "nope", fault.Spec{Type: fault.Reset}); !IsNotFound(err) {
		t.Fatal(err)
	}
	if _, err := c.AddFault(ctx, "x", fault.Spec{Type: "melt"}); status(err) != http.StatusBadRequest {
		t.Fatal(err)
	}

	raw := func(method, path, body string) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, method, c.Base()+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := raw("POST", "/v1/proxies", `{"name":"y","bogus":1}`); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	if code, _ := raw("POST", "/v1/proxies/x/faults", `not json`); code != 400 {
		t.Fatalf("bad json: %d", code)
	}
	if code, _ := raw("POST", "/v1/killswitch", `[]`); code != 400 {
		t.Fatalf("bad kill body: %d", code)
	}
	if code, _ := raw("GET", "/v1/audit?since=abc", ""); code != 400 {
		t.Fatalf("bad since: %d", code)
	}
}

func TestTokenAuth(t *testing.T) {
	c, _ := setup(t, "s3cret")
	ctx := t.Context()
	if _, err := c.Proxies(ctx); err != nil {
		t.Fatalf("authorised client rejected: %v", err)
	}
	anon := NewClient(strings.TrimPrefix(c.Base(), "http://"), "")
	if _, err := anon.Health(ctx); err != nil {
		t.Fatalf("health must stay open: %v", err)
	}
	var ae *APIError
	if _, err := anon.Proxies(ctx); !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", err)
	}
	wrong := NewClient(c.Base(), "nope")
	if _, err := wrong.KillSwitch(ctx); !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", err)
	}
}

func TestMetrics(t *testing.T) {
	c, up := setup(t, "")
	ctx := t.Context()
	_, _ = c.CreateProxy(ctx, engine.ProxySpec{Name: "db", Kind: fault.KindTCP, Upstream: up})
	_, _ = c.CreateProxy(ctx, engine.ProxySpec{Name: "edge", Kind: fault.KindHTTP, Upstream: "http://" + up})
	_, _ = c.AddFault(ctx, "db", fault.Spec{Type: fault.Reset})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.Base()+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		`faultline_active_faults{proxy="db"} 1`,
		`faultline_active_faults{proxy="edge"} 0`,
		`# TYPE faultline_tcp_connections_total counter`,
		`# TYPE faultline_active_faults gauge`,
		`faultline_http_requests_total{proxy="edge"} 0`,
		`faultline_killswitch_engaged 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestClientTransportErrors(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	c := NewClient(addr, "")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := c.Health(ctx); err == nil {
		t.Fatal("expected dial error")
	}
	// Non-JSON error bodies are surfaced verbatim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "plain failure", http.StatusTeapot)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "").Health(ctx)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Message != "plain failure" {
		t.Fatalf("got %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{not json")
	}))
	defer bad.Close()
	if _, err := NewClient(bad.URL, "").Health(ctx); err == nil {
		t.Fatal("expected decode error")
	}
}
