// Package app implements the command with injectable external providers.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/dashboard"
	"github.com/jeremyrickard/dalec-project-health/internal/inventory"
	"github.com/jeremyrickard/dalec-project-health/internal/lfx"
	"github.com/jeremyrickard/dalec-project-health/internal/netx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
	"github.com/jeremyrickard/dalec-project-health/internal/upstream"
)

var ErrPartial = errors.New("partial report saved: external enrichment failed")

type Dependencies struct {
	GitHub upstream.Provider
	LFX    lfx.Provider
	Now    func() time.Time
}

func Run(ctx context.Context, args []string, out, warnings io.Writer, deps Dependencies) error {
	if len(args) == 0 || args[0] != "generate" {
		return fmt.Errorf("usage: dalec-project-health generate --build-defs-dir <checkout> --output-dir <directory>")
	}
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(warnings)
	checkout := flags.String("build-defs-dir", "", "explicit input checkout; only specs/ is inventoried")
	output := flags.String("output-dir", "", "directory for index.html and project-health.json")
	timeout := flags.Duration("timeout", 30*time.Minute, "total inventory/enrichment timeout; partial output is still saved")
	requestTimeout := flags.Duration("request-timeout", netx.DefaultRequestTimeout, "GitHub per-attempt timeout, excluding rate-limit waits")
	upstreamTimeout := flags.Duration("upstream-timeout", upstream.DefaultUpstreamTimeout, "GitHub per-upstream working budget, excluding rate/queue waits")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *checkout == "" || *output == "" || flags.NArg() != 0 || *timeout <= 0 || *requestTimeout <= 0 || *upstreamTimeout <= 0 {
		return fmt.Errorf("--build-defs-dir and --output-dir are required; timeouts must be positive; positional arguments are not supported")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.GitHub == nil {
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			token = os.Getenv("GH_TOKEN")
		}
		deps.GitHub = upstream.NewGitHub(token, netx.NewGitHub(*requestTimeout))
	}
	if deps.LFX == nil {
		deps.LFX = lfx.New()
	}
	ctx, cancel := context.WithTimeoutCause(ctx, *timeout, netx.ErrRunDeadline)
	defer cancel()
	r, err := inventory.Load(ctx, *checkout, deps.Now())
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	lfx.Enrich(ctx, r, deps.LFX, deps.Now)
	upstream.EnrichWithTimeout(ctx, r, deps.GitHub, deps.Now, *upstreamTimeout)
	if ctx.Err() != nil {
		r.Failure("generation", "deadline or cancellation", netx.ContextError(ctx))
	}
	if err := Save(*output, r); err != nil {
		return fmt.Errorf("save report: %w", err)
	}
	count := 0
	for _, p := range r.Projects {
		for _, c := range p.Components {
			count += len(c.Versions)
		}
	}
	fmt.Fprintf(out, "Saved %d project groups, %d spec versions/variants to %s (partial=%t)\n", len(r.Projects), count, *output, r.Partial)
	for _, d := range r.Diagnostics {
		fmt.Fprintf(warnings, "warning: %s / %s: %s\n", d.Provider, d.Subject, d.Message)
	}
	if r.Partial {
		return ErrPartial
	}
	return nil
}

func Save(dir string, r *report.Report) error {
	js, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	var html bytes.Buffer
	if err := dashboard.Render(&html, r); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	type output struct {
		name    string
		content []byte
	}
	files := []output{{"project-health.json", append(js, '\n')}, {"index.html", html.Bytes()}}
	staged := []string{}
	defer func() {
		for _, path := range staged {
			os.Remove(path)
		}
	}()
	for _, file := range files {
		f, err := os.CreateTemp(dir, ".project-health-*")
		if err != nil {
			return err
		}
		staged = append(staged, f.Name())
		_, writeErr := f.Write(file.content)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	}
	for i, file := range files {
		if err := os.Rename(staged[i], filepath.Join(dir, file.name)); err != nil {
			return err
		}
	}
	return nil
}
