// Package cli implements the faultline command line: the control-plane
// daemon (serve), ad-hoc proxy and fault management against a running
// daemon, and the experiment runner.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/archit2501/faultline/internal/control"
)

// Environment variables read by the CLI.
const (
	EnvControl = "FAULTLINE_CONTROL"
	EnvToken   = "FAULTLINE_TOKEN"
)

// DefaultControl is the control API address used when neither --control
// nor FAULTLINE_CONTROL is set.
const DefaultControl = "127.0.0.1:7070"

// Exit codes not tied to an experiment verdict.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 64
)

// errUsage marks errors caused by bad command line input.
var errUsage = errors.New("usage error")

// env is the process environment the CLI runs in; tests substitute it.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, e *env, args []string) error
}

func commands() []command {
	return []command{
		{"serve", "run the control API and the proxies it manages", serve},
		{"health", "check that a control API is up (optionally wait for it)", health},
		{"proxy", "list, create or delete proxies: proxy ls | create | rm", proxyCmd},
		{"fault", "inject and remove faults: fault ls | add | rm | clear", faultCmd},
		{"killswitch", "engage or release the global kill switch: killswitch on | off | status", killCmd},
		{"audit", "print the control-plane audit log", auditCmd},
		{"run", "run an experiment file and write its report", runCmd},
		{"validate", "parse and validate experiment files without running them", validateCmd},
		{"version", "print the version", versionCmd},
	}
}

// Main runs the CLI with args (without the program name) and returns the
// process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return mainEnv(ctx, &env{stdout: stdout, stderr: stderr, getenv: os.Getenv}, args)
}

func mainEnv(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(e.stdout)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		err := c.run(ctx, e, args[1:])
		var ve verdictError
		switch {
		case err == nil:
			return exitOK
		case errors.Is(err, flag.ErrHelp):
			return exitOK
		case errors.As(err, &ve):
			return ve.code
		case errors.Is(err, errUsage):
			fmt.Fprintf(e.stderr, "faultline %s: %v\n", c.name, err)
			return exitUsage
		default:
			fmt.Fprintf(e.stderr, "faultline %s: %v\n", c.name, err)
			return exitError
		}
	}
	fmt.Fprintf(e.stderr, "faultline: unknown command %q\n\n", args[0])
	usage(e.stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "faultline: fault injection proxies and chaos experiments for local and staging systems")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: faultline <command> [flags] [args]")
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands() {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Client commands talk to --control (default $%s or %s) and send $%s as a bearer token.\n", EnvControl, DefaultControl, EnvToken)
	fmt.Fprintln(w, "Run 'faultline <command> -h' for the flags of a command.")
}

// verdictError carries an experiment verdict's exit code out of runCmd
// without printing anything further.
type verdictError struct{ code int }

func (v verdictError) Error() string { return fmt.Sprintf("exit status %d", v.code) }

// newFlags builds a flag set whose errors are returned, not printed and
// exited on, so commands are testable.
func newFlags(e *env, name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: faultline %s %s\n\nFlags:\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags that may appear before, between or after positional
// arguments and returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %w", errUsage, err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// wantArgs checks the positional argument count.
func wantArgs(pos []string, n int, what string) error {
	if len(pos) != n {
		return fmt.Errorf("%w: expected %s, got %d argument(s)", errUsage, what, len(pos))
	}
	return nil
}

// clientFlags registers --control and returns a constructor for the client.
func clientFlags(e *env, fs *flag.FlagSet) func() *control.Client {
	def := e.getenv(EnvControl)
	if def == "" {
		def = DefaultControl
	}
	addr := fs.String("control", def, "control API address or URL (env "+EnvControl+")")
	return func() *control.Client { return control.NewClient(*addr, e.getenv(EnvToken)) }
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(w io.Writer, header string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

func health(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "health", "[--wait 10s]")
	client := clientFlags(e, fs)
	wait := fs.Duration("wait", 0, "keep retrying for up to this long")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c := client()
	deadline := time.Now().Add(*wait)
	for {
		h, err := c.Health(ctx)
		if err == nil {
			fmt.Fprintf(e.stdout, "%s: %s (version %s, %d proxies, killswitch %v)\n", c.Base(), h.Status, h.Version, h.Proxies, h.KillSwitch)
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func versionCmd(_ context.Context, e *env, args []string) error {
	fs := newFlags(e, "version", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "faultline %s\n", control.Version)
	return nil
}

func auditCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "audit", "[--since N] [--json]")
	client := clientFlags(e, fs)
	since := fs.Int64("since", 0, "only events with a sequence number greater than this")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	evs, err := client().Audit(ctx, *since)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(e.stdout, evs)
	}
	rows := make([][]string, 0, len(evs))
	for _, ev := range evs {
		rows = append(rows, []string{
			strconv.FormatInt(ev.Seq, 10), ev.Time.Format("15:04:05.000"), ev.Action,
			dash(ev.Proxy), dash(ev.Fault), dash(ev.Detail),
		})
	}
	return table(e.stdout, "SEQ\tTIME\tACTION\tPROXY\tFAULT\tDETAIL", rows)
}

func killCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "killswitch", "on|off|status [--reason text]")
	client := clientFlags(e, fs)
	reason := fs.String("reason", "", "why the kill switch is engaged (recorded in the audit log)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, "on, off or status"); err != nil {
		return err
	}
	c := client()
	switch pos[0] {
	case "on":
		r := *reason
		if r == "" {
			r = "engaged from the CLI"
		}
		ks, err := c.SetKillSwitch(ctx, true, r)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "kill switch ENGAGED: %s (all faults removed; new faults refused)\n", ks.Reason)
	case "off":
		if _, err := c.SetKillSwitch(ctx, false, ""); err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, "kill switch released")
	case "status":
		ks, err := c.KillSwitch(ctx)
		if err != nil {
			return err
		}
		if ks.Engaged {
			fmt.Fprintf(e.stdout, "engaged since %s: %s\n", ks.Since.Format(time.RFC3339), ks.Reason)
		} else {
			fmt.Fprintln(e.stdout, "released")
		}
	default:
		return fmt.Errorf("%w: expected on, off or status, got %q", errUsage, pos[0])
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
