// Package control exposes the engine over a small JSON HTTP API and
// provides the matching Go client used by the CLI and the experiment
// runner.
package control

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/archit2501/faultline/internal/engine"
	"github.com/archit2501/faultline/internal/fault"
)

// maxBody bounds request bodies; fault specs are tiny.
const maxBody = 1 << 20

// Version is reported by the health endpoint.
var Version = "0.1.0"

// Health is the response of GET /v1/health.
type Health struct {
	Status     string `json:"status"`
	Version    string `json:"version"`
	Proxies    int    `json:"proxies"`
	KillSwitch bool   `json:"killswitch"`
}

// KillSwitchRequest is the body of POST /v1/killswitch.
type KillSwitchRequest struct {
	Engaged bool   `json:"engaged"`
	Reason  string `json:"reason,omitempty"`
}

// Removed is the response of bulk deletes.
type Removed struct {
	Removed int `json:"removed"`
}

type errorBody struct {
	Error string `json:"error"`
}

// HandlerOptions configures the control API.
type HandlerOptions struct {
	// Token, when non-empty, is required as "Authorization: Bearer <token>"
	// on every endpoint except /v1/health.
	Token  string
	Logger *slog.Logger
}

type server struct {
	eng   *engine.Engine
	token string
	log   *slog.Logger
}

// NewHandler returns the control API handler for eng.
func NewHandler(eng *engine.Engine, opts HandlerOptions) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &server{eng: eng, token: opts.Token, log: opts.Logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/proxies", s.auth(s.listProxies))
	mux.HandleFunc("POST /v1/proxies", s.auth(s.createProxy))
	mux.HandleFunc("GET /v1/proxies/{name}", s.auth(s.getProxy))
	mux.HandleFunc("DELETE /v1/proxies/{name}", s.auth(s.deleteProxy))
	mux.HandleFunc("GET /v1/proxies/{name}/faults", s.auth(s.listFaults))
	mux.HandleFunc("POST /v1/proxies/{name}/faults", s.auth(s.addFault))
	mux.HandleFunc("DELETE /v1/proxies/{name}/faults", s.auth(s.clearFaults))
	mux.HandleFunc("DELETE /v1/proxies/{name}/faults/{id}", s.auth(s.removeFault))
	mux.HandleFunc("DELETE /v1/faults", s.auth(s.clearAll))
	mux.HandleFunc("GET /v1/killswitch", s.auth(s.getKill))
	mux.HandleFunc("POST /v1/killswitch", s.auth(s.setKill))
	mux.HandleFunc("GET /v1/audit", s.auth(s.audit))
	mux.HandleFunc("GET /metrics", s.auth(s.metrics))
	return mux
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	if s.token == "" {
		return h
	}
	want := []byte("Bearer " + s.token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "missing or invalid bearer token"})
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, engine.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, engine.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, engine.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, engine.ErrBlocked):
		status = http.StatusForbidden
	case errors.Is(err, engine.ErrKillSwitch):
		status = http.StatusLocked
	}
	s.log.Info("control request rejected", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	writeJSON(w, status, errorBody{Error: err.Error()})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: decode body: %w", engine.ErrInvalid, err)
	}
	return nil
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{
		Status: "ok", Version: Version,
		Proxies: len(s.eng.Proxies()), KillSwitch: s.eng.KillSwitch().Engaged,
	})
}

func (s *server) listProxies(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.Proxies())
}

func (s *server) createProxy(w http.ResponseWriter, r *http.Request) {
	var spec engine.ProxySpec
	if err := decode(r, &spec); err != nil {
		s.fail(w, r, err)
		return
	}
	info, err := s.eng.CreateProxy(spec)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *server) getProxy(w http.ResponseWriter, r *http.Request) {
	info, err := s.eng.Proxy(r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DeleteProxy(r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listFaults(w http.ResponseWriter, r *http.Request) {
	info, err := s.eng.Proxy(r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info.Faults)
}

func (s *server) addFault(w http.ResponseWriter, r *http.Request) {
	var spec fault.Spec
	if err := decode(r, &spec); err != nil {
		s.fail(w, r, err)
		return
	}
	info, err := s.eng.AddFault(r.PathValue("name"), spec)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *server) clearFaults(w http.ResponseWriter, r *http.Request) {
	n, err := s.eng.ClearFaults(r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, Removed{Removed: n})
}

func (s *server) removeFault(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.RemoveFault(r.PathValue("name"), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) clearAll(w http.ResponseWriter, _ *http.Request) {
	n := s.eng.ClearAll()
	s.eng.Audit().Record(engine.Event{Action: "fault.clear_all", Detail: fmt.Sprintf("%d removed", n)})
	writeJSON(w, http.StatusOK, Removed{Removed: n})
}

func (s *server) getKill(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.KillSwitch())
}

func (s *server) setKill(w http.ResponseWriter, r *http.Request) {
	var req KillSwitchRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Engaged && req.Reason == "" {
		req.Reason = "engaged via control API"
	}
	writeJSON(w, http.StatusOK, s.eng.SetKillSwitch(req.Engaged, req.Reason))
}

func (s *server) audit(w http.ResponseWriter, r *http.Request) {
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: since must be an integer", engine.ErrInvalid))
			return
		}
		since = n
	}
	writeJSON(w, http.StatusOK, s.eng.Audit().Events(since))
}

// metrics renders counters in the Prometheus text exposition format.
func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	type sample struct {
		name, help string
		values     map[string]int64
	}
	series := map[string]*sample{}
	order := []string{}
	add := func(name, help, proxy string, v int64) {
		sm, ok := series[name]
		if !ok {
			sm = &sample{name: name, help: help, values: map[string]int64{}}
			series[name] = sm
			order = append(order, name)
		}
		sm.values[proxy] = v
	}
	for _, p := range s.eng.Proxies() {
		add("faultline_active_faults", "Faults currently installed.", p.Name, int64(len(p.Faults)))
		if st := p.TCP; st != nil {
			add("faultline_tcp_connections_total", "Accepted TCP connections.", p.Name, st.Accepted)
			add("faultline_tcp_connections_active", "Open TCP connections.", p.Name, st.Active)
			add("faultline_tcp_bytes_upstream_total", "Bytes relayed client to upstream.", p.Name, st.BytesUpstream)
			add("faultline_tcp_bytes_downstream_total", "Bytes relayed upstream to client.", p.Name, st.BytesDownstream)
			add("faultline_tcp_bytes_dropped_total", "Bytes discarded by blackhole faults.", p.Name, st.BytesDropped)
			add("faultline_tcp_resets_total", "Connections reset by faults.", p.Name, st.Resets)
			add("faultline_tcp_closes_total", "Connections closed by faults.", p.Name, st.Closes)
			add("faultline_tcp_dial_errors_total", "Failed upstream dials.", p.Name, st.DialErrors)
		}
		if st := p.HTTP; st != nil {
			add("faultline_http_requests_total", "Proxied HTTP requests.", p.Name, st.Requests)
			add("faultline_http_faulted_total", "Requests at least one fault acted on.", p.Name, st.Faulted)
			add("faultline_http_status_injected_total", "Requests answered with an injected status.", p.Name, st.StatusInjected)
			add("faultline_http_aborted_total", "Requests whose connection was aborted.", p.Name, st.Aborted)
			add("faultline_http_corrupted_total", "Responses with corrupted bodies.", p.Name, st.Corrupted)
			add("faultline_http_upstream_errors_total", "Requests that failed upstream.", p.Name, st.UpstreamErrors)
		}
	}
	for _, name := range order {
		sm := series[name]
		kind := "counter"
		if !strings.HasSuffix(name, "_total") {
			kind = "gauge"
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, sm.help, name, kind)
		proxies := make([]string, 0, len(sm.values))
		for p := range sm.values {
			proxies = append(proxies, p)
		}
		sort.Strings(proxies)
		for _, p := range proxies {
			fmt.Fprintf(&b, "%s{proxy=%q} %d\n", name, p, sm.values[p])
		}
	}
	kill := 0
	if s.eng.KillSwitch().Engaged {
		kill = 1
	}
	fmt.Fprintf(&b, "# HELP faultline_killswitch_engaged 1 when the kill switch is engaged.\n# TYPE faultline_killswitch_engaged gauge\nfaultline_killswitch_engaged %d\n", kill)
	_, _ = io.WriteString(w, b.String())
}
