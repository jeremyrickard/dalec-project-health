package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/lfx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
	"github.com/jeremyrickard/dalec-project-health/internal/upstream"
)

type gitFixture struct{ fail bool }

func (g gitFixture) Snapshot(context.Context, string) (upstream.Snapshot, error) {
	if g.fail {
		return upstream.Snapshot{}, fmt.Errorf("synthetic provider unavailable")
	}
	date := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return upstream.Snapshot{
		Tags:     []upstream.Tag{{Name: "v1.2.3"}, {Name: "v1.2.9"}, {Name: "v1.8.0"}, {Name: "agent/v2.4.0"}},
		Releases: []upstream.Release{{Tag: "v1.2.3", Date: &date}, {Tag: "v1.2.9", Date: &date}, {Tag: "agent/v2.4.0", Date: &date}},
	}, nil
}
func (gitFixture) Commit(context.Context, string, string) (upstream.Commit, error) {
	return upstream.Commit{SHA: "synthetic-commit"}, nil
}

type lfxFixture struct{}

func (lfxFixture) List(context.Context, []string) ([]lfx.Project, error) {
	return []lfx.Project{{Slug: "widget", Name: "Synthetic Widget", GitHub: "https://github.com/example/widget"}}, nil
}
func (lfxFixture) Insights(context.Context, string) (lfx.Insight, error) {
	score, maxScore, count, label, lifecycle := 52.0, 65.0, 2, "healthy", "stable"
	return lfx.Insight{Slug: "widget", Name: "Synthetic Widget", Type: "project", Score: &score, MaxScore: &maxScore, Coverage: &count, Label: &label, Lifecycle: &lifecycle}, nil
}

func fixtureDeps(fail bool) Dependencies {
	return Dependencies{GitHub: gitFixture{fail}, LFX: lfxFixture{}, Now: func() time.Time { return time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC) }}
}

func TestGenerateFixture(t *testing.T) {
	dir := t.TempDir()
	var out, warnings bytes.Buffer
	if err := Run(context.Background(), []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", dir}, &out, &warnings, fixtureDeps(false)); err != nil {
		t.Fatal(err)
	}
	dt, err := os.ReadFile(filepath.Join(dir, "project-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r report.Report
	if err := json.Unmarshal(dt, &r); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range r.Projects {
		for _, c := range p.Components {
			count += len(c.Versions)
		}
	}
	if count != 5 || len(r.Projects) != 4 || r.Partial {
		t.Fatalf("bad smoke counts: projects=%d versions=%d partial=%t", len(r.Projects), count, r.Partial)
	}
	if smokeDir := os.Getenv("DALEC_HEALTH_SMOKE_DIR"); smokeDir != "" {
		if err := Save(smokeDir, &r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPartialReportSavedBeforeError(t *testing.T) {
	dir := t.TempDir()
	var out, warnings bytes.Buffer
	err := Run(context.Background(), []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", dir}, &out, &warnings, fixtureDeps(true))
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("got %v", err)
	}
	dt, err := os.ReadFile(filepath.Join(dir, "project-health.json"))
	if err != nil {
		t.Fatal("partial report not saved", err)
	}
	if !bytes.Contains(dt, []byte(`"partial": true`)) || warnings.Len() == 0 {
		t.Fatal("partial status/warnings missing")
	}
}

type cancellingGit struct{ cancel context.CancelFunc }

func (g cancellingGit) Snapshot(context.Context, string) (upstream.Snapshot, error) {
	g.cancel()
	return upstream.Snapshot{}, context.Canceled
}
func (cancellingGit) Commit(context.Context, string, string) (upstream.Commit, error) {
	return upstream.Commit{}, context.Canceled
}

func TestExternalCancellationPreservesInventoryAndReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := fixtureDeps(false)
	deps.GitHub = cancellingGit{cancel}
	dir := t.TempDir()
	err := Run(ctx, []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", dir}, &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("got %v", err)
	}
	dt, err := os.ReadFile(filepath.Join(dir, "project-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r report.Report
	if err := json.Unmarshal(dt, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Projects) != 4 || !r.Partial {
		t.Fatal("cancellation lost inventory")
	}
	found := false
	for _, d := range r.Diagnostics {
		found = found || d.Provider == "generation" && d.Message == context.Canceled.Error()
	}
	if !found {
		t.Fatal("cancellation reason not preserved")
	}
}

func TestCommandAndOutputFailures(t *testing.T) {
	for _, args := range [][]string{nil, {"other"}, {"generate"}, {"generate", "--build-defs-dir", "testdata/fixture"}, {"generate", "--bad"}, {"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", t.TempDir(), "--timeout", "0s"}} {
		if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, fixtureDeps(false)); err == nil {
			t.Errorf("accepted invalid args %v", args)
		}
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(file, report.New(time.Now())); err == nil {
		t.Fatal("output failure hidden")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "index.html"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, warnings bytes.Buffer
	if err := Run(context.Background(), []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", dir}, &out, &warnings, fixtureDeps(false)); err == nil || out.Len() != 0 {
		t.Fatal("rename failure was reported as success")
	}
}
