package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v84/github"
	"github.com/jeremyrickard/dalec-project-health/internal/lfx"
	"github.com/jeremyrickard/dalec-project-health/internal/netx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
	"github.com/jeremyrickard/dalec-project-health/internal/upstream"
)

type gitFixture struct{ fail bool }

func (g gitFixture) Snapshot(_ context.Context, repository string) (upstream.Snapshot, error) {
	if g.fail {
		return upstream.Snapshot{}, fmt.Errorf("synthetic provider unavailable")
	}
	date := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	s := upstream.Snapshot{
		DefaultBranch: "main", ReleasesComplete: true,
		Tags:     []upstream.Tag{{Name: "v1.2.3"}, {Name: "v1.2.9"}, {Name: "v1.8.0"}, {Name: "agent/v2.4.0"}},
		Releases: []upstream.Release{{Tag: "v1.2.3", Date: &date}, {Tag: "v1.2.9", Date: &date}, {Tag: "agent/v2.4.0", Date: &date}},
	}
	if strings.HasSuffix(repository, "/quiet") {
		s.Releases = nil
	}
	return s, nil
}
func (gitFixture) Commit(context.Context, string, string) (upstream.Commit, error) {
	return upstream.Commit{SHA: "synthetic-commit"}, nil
}
func (g gitFixture) Activity(_ context.Context, repository string, s upstream.Snapshot, asOf time.Time) (report.Activity, error) {
	if g.fail {
		return upstream.SummarizeActivity(s, nil, asOf, asOf, false), fmt.Errorf("synthetic activity unavailable")
	}
	commits := []*github.RepositoryCommit{}
	if strings.HasSuffix(repository, "/agent") || strings.HasSuffix(repository, "/partial") {
		for i := range 12 {
			commits = append(commits, &github.RepositoryCommit{
				SHA: github.Ptr(fmt.Sprintf("synthetic-%d", i)), Parents: []*github.Commit{},
				Author: &github.User{ID: github.Ptr(int64(1)), Login: github.Ptr("synthetic-author"), Type: github.Ptr("User")},
				Commit: &github.Commit{Committer: &github.CommitAuthor{Date: &github.Timestamp{Time: asOf.Add(-time.Duration(1+i%3*30) * 24 * time.Hour)}}},
			})
		}
	}
	complete := !strings.HasSuffix(repository, "/partial")
	a := upstream.SummarizeActivity(s, commits, asOf, asOf, complete)
	a.HeadSHA = "synthetic-main-head"
	a.EvidenceURL = repository + "/commits/" + a.HeadSHA
	a.ReleaseEvidenceURL = repository + "/releases"
	if !complete {
		return a, fmt.Errorf("synthetic pagination interrupted; observed counts are lower bounds")
	}
	return a, nil
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
	if err := Run(context.Background(), []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", dir}, &out, &warnings, fixtureDeps(false)); !errors.Is(err, ErrPartial) {
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
	bands := map[string]int{}
	for _, p := range r.Projects {
		if p.Activity != nil {
			bands[p.Activity.Band]++
		} else if p.Repository != "https://github.com/example/widget" {
			t.Fatal("only complete native LFX should skip activity")
		}
		for _, c := range p.Components {
			count += len(c.Versions)
			if c.Name == "synthetic-worker" {
				v := c.Versions[0]
				if p.Repository != "" || v.Ref != "" || len(v.Candidates) != 0 || len(v.Images) != 3 {
					t.Fatal("source-free frontend worker lost images or acquired invented source provenance")
				}
			}
		}
	}
	if count != 9 || len(r.Projects) != 8 || !r.Partial {
		t.Fatalf("bad smoke counts: projects=%d versions=%d partial=%t", len(r.Projects), count, r.Partial)
	}
	if r.SchemaVersion != "2" || bands["Sustained activity"] != 1 || bands["Some activity"] != 1 || bands["No observed activity"] != 1 || bands[report.Unknown] != 4 {
		t.Fatalf("bad activity smoke coverage: %v", bands)
	}
	if bytes.Contains(dt, []byte("synthetic-author")) {
		t.Fatal("transient contributor identity persisted")
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
func (cancellingGit) Activity(_ context.Context, _ string, s upstream.Snapshot, asOf time.Time) (report.Activity, error) {
	return upstream.SummarizeActivity(s, nil, asOf, asOf, false), context.Canceled
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
	if len(r.Projects) != 8 || !r.Partial {
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
	for _, name := range []string{"--timeout", "--request-timeout", "--upstream-timeout"} {
		for _, duration := range []string{"0s", "-1s"} {
			args := []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", t.TempDir(), name, duration}
			if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, fixtureDeps(false)); err == nil {
				t.Errorf("accepted invalid %s %s", name, duration)
			}
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

type budgetGit struct{}

func (budgetGit) Snapshot(ctx context.Context, _ string) (upstream.Snapshot, error) {
	<-ctx.Done()
	return upstream.Snapshot{}, netx.ContextError(ctx)
}
func (budgetGit) Commit(ctx context.Context, _, _ string) (upstream.Commit, error) {
	return upstream.Commit{}, netx.ContextError(ctx)
}
func (budgetGit) Activity(ctx context.Context, _ string, s upstream.Snapshot, asOf time.Time) (report.Activity, error) {
	return upstream.SummarizeActivity(s, nil, asOf, asOf, false), netx.ContextError(ctx)
}

func TestConfiguredBudgetsSaveExplicitPartialReport(t *testing.T) {
	for _, scope := range []string{"run", "upstream"} {
		t.Run(scope, func(t *testing.T) {
			args := []string{"generate", "--build-defs-dir", "testdata/fixture", "--output-dir", t.TempDir()}
			want := "generation/run deadline exhausted"
			if scope == "run" {
				args = append(args, "--timeout", "200ms")
			} else {
				args = append(args, "--timeout", "2s", "--upstream-timeout", "20ms")
				want = "upstream working budget exhausted"
			}
			deps := fixtureDeps(false)
			deps.GitHub = budgetGit{}
			var out, warnings bytes.Buffer
			err := Run(context.Background(), args, &out, &warnings, deps)
			if !errors.Is(err, ErrPartial) || !strings.Contains(warnings.String(), want) {
				t.Fatalf("deadline scope or partial exit lost: %v / %s", err, &warnings)
			}
			data, err := os.ReadFile(filepath.Join(args[4], "project-health.json"))
			if err != nil {
				t.Fatal(err)
			}
			var r report.Report
			if err := json.Unmarshal(data, &r); err != nil || len(r.Projects) != 8 || !r.Partial {
				t.Fatalf("deadline discarded local inventory: %v", err)
			}
			if scope == "upstream" && strings.Contains(warnings.String(), "generation/run deadline") {
				t.Fatal("upstream timeout incorrectly exhausted the whole run")
			}
		})
	}
}
