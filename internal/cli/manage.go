package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/archit2501/faultline/internal/engine"
	"github.com/archit2501/faultline/internal/fault"
)

func proxyCmd(ctx context.Context, e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: expected a subcommand: ls, create or rm", errUsage)
	}
	switch args[0] {
	case "ls", "list":
		return proxyList(ctx, e, args[1:])
	case "create":
		return proxyCreate(ctx, e, args[1:])
	case "rm", "delete":
		return proxyDelete(ctx, e, args[1:])
	}
	return fmt.Errorf("%w: unknown proxy subcommand %q", errUsage, args[0])
}

func proxyList(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "proxy ls", "[--json]")
	client := clientFlags(e, fs)
	asJSON := fs.Bool("json", false, "print JSON including per-proxy counters")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	ps, err := client().Proxies(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(e.stdout, ps)
	}
	rows := make([][]string, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, []string{p.Name, string(p.Kind), p.Listen, p.Upstream, strconv.Itoa(len(p.Faults)), traffic(p)})
	}
	return table(e.stdout, "NAME\tKIND\tLISTEN\tUPSTREAM\tFAULTS\tTRAFFIC", rows)
}

func traffic(p engine.ProxyInfo) string {
	if st := p.TCP; st != nil {
		return fmt.Sprintf("%d conns, %d B up, %d B down, %d resets", st.Accepted, st.BytesUpstream, st.BytesDownstream, st.Resets)
	}
	if st := p.HTTP; st != nil {
		return fmt.Sprintf("%d reqs, %d faulted", st.Requests, st.Faulted)
	}
	return "-"
}

func proxyCreate(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "proxy create", "NAME --kind tcp|http --upstream ADDR [--listen ADDR]")
	client := clientFlags(e, fs)
	kind := fs.String("kind", "tcp", "proxy kind: tcp (L4) or http (L7)")
	listen := fs.String("listen", "127.0.0.1:0", "listen address")
	upstream := fs.String("upstream", "", "upstream host:port (tcp) or base URL (http)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, "a proxy NAME"); err != nil {
		return err
	}
	if *upstream == "" {
		return fmt.Errorf("%w: --upstream is required", errUsage)
	}
	info, err := client().CreateProxy(ctx, engine.ProxySpec{Name: pos[0], Kind: fault.Kind(*kind), Listen: *listen, Upstream: *upstream})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "proxy %s (%s) listening on %s -> %s\n", info.Name, info.Kind, info.Listen, info.Upstream)
	return nil
}

func proxyDelete(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "proxy rm", "NAME")
	client := clientFlags(e, fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, "a proxy NAME"); err != nil {
		return err
	}
	if err := client().DeleteProxy(ctx, pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "proxy %s deleted\n", pos[0])
	return nil
}

func faultCmd(ctx context.Context, e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: expected a subcommand: ls, add, rm or clear", errUsage)
	}
	switch args[0] {
	case "ls", "list":
		return faultList(ctx, e, args[1:])
	case "add":
		return faultAdd(ctx, e, args[1:])
	case "rm", "remove":
		return faultRemove(ctx, e, args[1:])
	case "clear":
		return faultClear(ctx, e, args[1:])
	}
	return fmt.Errorf("%w: unknown fault subcommand %q", errUsage, args[0])
}

func faultList(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "fault ls", "PROXY [--json]")
	client := clientFlags(e, fs)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, "a PROXY name"); err != nil {
		return err
	}
	fsInfo, err := client().Faults(ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(e.stdout, fsInfo)
	}
	rows := make([][]string, 0, len(fsInfo))
	now := time.Now()
	for _, f := range fsInfo {
		expires := "never"
		if f.ExpiresAt != nil {
			expires = "in " + f.ExpiresAt.Sub(now).Round(time.Second).String()
		}
		rows = append(rows, []string{f.ID, f.Describe(), expires, strconv.FormatInt(f.Applied, 10), strconv.FormatInt(f.Affected, 10)})
	}
	return table(e.stdout, "ID\tFAULT\tEXPIRES\tAPPLIED\tAFFECTED NOW", rows)
}

// kvFlag collects repeated KEY=VALUE flags.
type kvFlag map[string]string

func (k kvFlag) String() string {
	parts := make([]string, 0, len(k))
	for _, key := range sortedKeys(k) {
		parts = append(parts, key+"="+k[key])
	}
	return strings.Join(parts, ",")
}

func (k kvFlag) Set(v string) error {
	key, val, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", v)
	}
	k[key] = val
	return nil
}

// durationFlag is a fault.Duration settable from the command line.
type durationFlag struct{ d *fault.Duration }

func (f durationFlag) String() string {
	if f.d == nil {
		return "0s"
	}
	return f.d.String()
}

func (f durationFlag) Set(v string) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	*f.d = fault.Duration(d)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// faultFlags binds a fault.Spec to flags and returns a finaliser that
// applies the list- and map-valued ones.
func faultFlags(fs *flag.FlagSet, s *fault.Spec) func() {
	fs.StringVar(&s.ID, "id", "", "fault ID (default: generated)")
	fs.Func("type", "fault type: latency|bandwidth|reset|close|slow_close|blackhole (tcp), delay|status|abort|corrupt (http)", func(v string) error {
		s.Type = fault.Type(v)
		return nil
	})
	fs.Func("direction", "tcp: upstream|downstream|both", func(v string) error {
		s.Direction = fault.Direction(v)
		return nil
	})
	fs.Float64Var(&s.Probability, "probability", 0, "chance a connection/request is affected, (0,1]; default 1")
	fs.Var(durationFlag{&s.TTL}, "ttl", "fault lifetime (capped by the server's --max-ttl)")
	fs.IntVar(&s.MaxAffected, "max-affected", 0, "cap on connections/requests affected at once (0: no cap)")
	sources := fs.String("source", "", "comma separated client IPs/CIDRs the fault is limited to")
	fs.Var(durationFlag{&s.Latency}, "latency", "latency (tcp) or delay (http) to add")
	fs.Var(durationFlag{&s.Jitter}, "jitter", "uniform jitter around --latency")
	fs.Int64Var(&s.RateBPS, "rate-bps", 0, "bandwidth cap in bytes per second")
	fs.Int64Var(&s.AfterBytes, "after-bytes", 0, "close/truncate after this many bytes")
	fs.Var(durationFlag{&s.Delay}, "delay", "reset after / slow-close hold time")
	fs.IntVar(&s.Status, "status", 0, "HTTP status to inject")
	fs.StringVar(&s.Body, "body", "", "response body for injected statuses")
	respHeaders := kvFlag{}
	fs.Var(respHeaders, "header", "response header for injected statuses, KEY=VALUE (repeatable)")
	fs.StringVar(&s.Mode, "mode", "", "corrupt mode: flip|truncate")
	fs.Float64Var(&s.Rate, "rate", 0, "corrupt flip rate per byte (default 0.01)")
	methods := fs.String("match-method", "", "http: comma separated methods to affect")
	path := fs.String("match-path", "", "http: path prefix to affect")
	matchHeaders := kvFlag{}
	fs.Var(matchHeaders, "match-header", "http: request header that must match, KEY=VALUE (repeatable)")
	return func() {
		s.Sources = splitList(*sources)
		if len(respHeaders) > 0 {
			s.ResponseHeaders = respHeaders
		}
		if *methods != "" || *path != "" || len(matchHeaders) > 0 {
			s.Match = &fault.HTTPMatch{Methods: splitList(*methods), PathPrefix: *path}
			if len(matchHeaders) > 0 {
				s.Match.Headers = matchHeaders
			}
		}
	}
}

func faultAdd(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "fault add", "PROXY --type TYPE [fault flags] | PROXY --json SPEC")
	client := clientFlags(e, fs)
	var spec fault.Spec
	finish := faultFlags(fs, &spec)
	raw := fs.String("json", "", "full fault spec as JSON (instead of the individual flags)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, "a PROXY name"); err != nil {
		return err
	}
	finish()
	switch {
	case *raw != "" && spec.Type != "":
		return fmt.Errorf("%w: use either --json or --type, not both", errUsage)
	case *raw != "":
		dec := json.NewDecoder(strings.NewReader(*raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return fmt.Errorf("%w: --json: %w", errUsage, err)
		}
	case spec.Type == "":
		return fmt.Errorf("%w: --type (or --json) is required", errUsage)
	}
	info, err := client().AddFault(ctx, pos[0], spec)
	if err != nil {
		return err
	}
	expires := "never"
	if info.ExpiresAt != nil {
		expires = info.ExpiresAt.Format(time.RFC3339)
	}
	fmt.Fprintf(e.stdout, "fault %s on %s: %s (expires %s)\n", info.ID, pos[0], info.Describe(), expires)
	return nil
}

func faultRemove(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "fault rm", "PROXY ID")
	client := clientFlags(e, fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 2, "PROXY and fault ID"); err != nil {
		return err
	}
	if err := client().RemoveFault(ctx, pos[0], pos[1]); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "fault %s removed from %s\n", pos[1], pos[0])
	return nil
}

func faultClear(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "fault clear", "PROXY | --all")
	client := clientFlags(e, fs)
	all := fs.Bool("all", false, "clear the faults of every proxy")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c := client()
	var n int
	switch {
	case *all && len(pos) == 0:
		n, err = c.ClearAll(ctx)
	case !*all && len(pos) == 1:
		n, err = c.ClearFaults(ctx, pos[0])
	default:
		return fmt.Errorf("%w: give exactly one of PROXY or --all", errUsage)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%d fault(s) removed\n", n)
	return nil
}
