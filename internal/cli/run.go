package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/archit2501/faultline/internal/control"
	"github.com/archit2501/faultline/internal/experiment"
)

func runCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlags(e, "run", "EXPERIMENT.yaml [--out DIR] [--control ADDR]")
	out := fs.String("out", "", "directory for report.json and report.md (default faultline-reports/<experiment>)")
	ctl := fs.String("control", "", "override the experiment's control API address")
	asJSON := fs.Bool("json", false, "print the JSON report instead of Markdown")
	quiet := fs.Bool("quiet", false, "print only the verdict line")
	level := fs.String("log-level", "warn", "log level for run progress: debug, info, warn, error")
	format := fs.String("log-format", "text", "log format: text or json")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := wantArgs(pos, 1, "one EXPERIMENT file"); err != nil {
		return err
	}
	log, err := newLogger(e, *level, *format)
	if err != nil {
		return err
	}
	spec, err := experiment.Load(pos[0])
	if err != nil {
		return fmt.Errorf("%s: %w", pos[0], err)
	}
	addr := spec.Control
	if *ctl != "" {
		addr = *ctl
	}
	client := control.NewClient(addr, e.getenv(EnvToken))
	runner := &experiment.Runner{Control: client, Logger: log, IsNotFound: control.IsNotFound}
	rep := runner.Run(ctx, spec)

	dir := *out
	if dir == "" {
		dir = filepath.Join("faultline-reports", spec.Name)
	}
	jsonPath, mdPath, werr := rep.WriteFiles(dir)
	switch {
	case *asJSON:
		if err := printJSON(e.stdout, rep); err != nil {
			return err
		}
	case !*quiet:
		rep.Markdown(e.stdout)
		fmt.Fprintln(e.stdout)
	}
	fmt.Fprintf(e.stderr, "verdict: %s (exit %d) in %s\n", strings.ToUpper(string(rep.Verdict)), rep.Verdict.ExitCode(), rep.EndedAt.Sub(rep.StartedAt).Round(time.Millisecond))
	if werr != nil {
		return fmt.Errorf("write report: %w", werr)
	}
	fmt.Fprintf(e.stderr, "report: %s, %s\n", mdPath, jsonPath)
	if code := rep.Verdict.ExitCode(); code != 0 {
		return verdictError{code: code}
	}
	return nil
}

func validateCmd(_ context.Context, e *env, args []string) error {
	fs := newFlags(e, "validate", "EXPERIMENT.yaml...")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("%w: expected at least one EXPERIMENT file", errUsage)
	}
	failed := 0
	for _, path := range pos {
		spec, err := experiment.Load(path)
		if err != nil {
			failed++
			fmt.Fprintf(e.stdout, "FAIL %s\n  %s\n", path, strings.ReplaceAll(err.Error(), "\n", "\n  "))
			continue
		}
		faults := 0
		for _, st := range spec.Method {
			faults += len(st.Faults)
		}
		fmt.Fprintf(e.stdout, "ok   %s: %q, %d probe(s), %d step(s), %d fault(s), schedule %s, abort objectives %d\n",
			path, spec.Name, len(spec.Probes), len(spec.Method), faults, spec.MethodDuration(), len(spec.Abort.Objectives))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d experiment file(s) invalid", failed, len(pos))
	}
	return nil
}
