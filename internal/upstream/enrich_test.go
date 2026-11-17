package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

func TestResolveStreamsVersionsAndAges(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	date := now.Add(-48 * time.Hour)
	s := Snapshot{Tags: []Tag{
		{"widget/v1.2.3", "abc"}, {"widget/v1.2.9", "def"}, {"widget/v1.10.0", "ghi"},
		{"widget/v2.0.0", "jkl"}, {"other/v9.0.0", "xyz"}, {"widget/v3.0.0-rc.1", "pre"},
		{"widget/v4.0.0", "flagged"},
	}, Releases: []Release{{Tag: "widget/v1.2.3", Date: &date}, {Tag: "widget/v4.0.0", Unstable: true}}}
	f := Resolve(s, "widget/v1.2.3", Commit{SHA: "abc"}, "https://github.com/example/repo", now)
	if f.ResolvedTag != "widget/v1.2.3" || f.LatestStable != "widget/v2.0.0" || f.LatestInLine != "widget/v1.2.9" || f.Update != "Major" || !f.LineUpdate || f.AgeDays == nil || *f.AgeDays != 2 || f.AgeKind != "release age" {
		t.Fatalf("bad stream comparison %+v", f)
	}
	f = Resolve(s, "abc", Commit{SHA: "abc", Date: &date}, "https://github.com/example/repo", now)
	if f.ResolvedTag != "widget/v1.2.3" {
		t.Fatal("commit association failed")
	}
	s.Tags = append(s.Tags, Tag{"other/v1.0.0", "abc"})
	f = Resolve(s, "abc", Commit{SHA: "abc", Date: &date}, "https://github.com/example/repo", now)
	if f.ResolvedTag != "" || f.Update != report.Unknown || f.AgeKind != "commit age" {
		t.Fatalf("ambiguous streams should be unknown: %+v", f)
	}
	f = Resolve(Snapshot{}, "unrelated", Commit{}, "", now)
	if f.Date != nil || f.AgeDays != nil || f.Update != report.Unknown {
		t.Fatal("fabricated age or update")
	}
}

func TestNumericalOrderingAndUpdateTypes(t *testing.T) {
	for _, tc := range []struct{ latest, update string }{
		{"v1.2.10", "Patch"}, {"v1.10.0", "Minor"}, {"v2.0.0", "Major"}, {"v1.2.3", "Current"},
	} {
		t.Run(tc.latest, func(t *testing.T) {
			s := Snapshot{Tags: []Tag{{Name: "v1.2.3"}, {Name: tc.latest}}}
			f := Resolve(s, "v1.2.3", Commit{}, "", time.Now())
			if f.Update != tc.update {
				t.Fatalf("got %s, want %s", f.Update, tc.update)
			}
		})
	}
}

func TestTagShapeDoesNotInventVersionStreams(t *testing.T) {
	for _, tag := range []string{"v10.2.3.4", "1.2.3.4", "release-final", "1.2", "v01.2.3"} {
		if _, version := splitTag(tag); version != nil {
			t.Errorf("non-semver tag %q was interpreted as %s", tag, version)
		}
	}
	for _, tag := range []string{"go1.26.1", "component/v1.2.3", "component-v1.2.3", "v1.2.3"} {
		if _, version := splitTag(tag); version == nil {
			t.Errorf("valid tag stream %q not recognized", tag)
		}
	}
}

func TestDateFallbackFutureDateAndNonSemver(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	past, future := now.Add(-72*time.Hour), now.Add(24*time.Hour)
	s := Snapshot{Tags: []Tag{{Name: "v1.0.0"}, {Name: "release-final"}}}
	f := Resolve(s, "v1.0.0", Commit{SHA: "abc", Date: &past}, "https://github.com/example/repo", now)
	if f.AgeKind != "commit age" || f.AgeDays == nil || *f.AgeDays != 3 {
		t.Fatal("commit age fallback is not explicit")
	}
	f = Resolve(s, "v1.0.0", Commit{SHA: "abc", Date: &future}, "", now)
	if f.AgeDays != nil || !strings.Contains(f.Reason, "future") {
		t.Fatal("future date was converted to a known age")
	}
	f = Resolve(s, "release-final", Commit{}, "", now)
	if f.ResolvedTag != "release-final" || f.Update != report.Unknown {
		t.Fatal("non-semver stream assigned a stable comparison")
	}
}

func TestGitHubPaginationAndCommit(t *testing.T) {
	tagPages, releasePages := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags"):
			tagPages++
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/tags?page=2>; rel=\"next\"", server.URL))
				fmt.Fprint(w, `[{"name":"v1.0.0","commit":{"sha":"abc"}}]`)
			} else {
				fmt.Fprint(w, `[{"name":"v1.1.0","commit":{"sha":"def"}}]`)
			}
		case strings.HasSuffix(r.URL.Path, "/releases"):
			releasePages++
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/releases?page=2>; rel=\"next\"", server.URL))
			}
			fmt.Fprintf(w, `[{"id":%d,"tag_name":"v1.%d.0","published_at":"2026-10-01T00:00:00Z"}]`, releasePages, releasePages-1)
		case strings.HasSuffix(r.URL.Path, "/branches/trunk"):
			fmt.Fprint(w, `{"name":"trunk","commit":{"sha":"pinned"}}`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprint(w, `{"sha":"abc","commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`)
		default:
			fmt.Fprint(w, `{"archived":false,"default_branch":"trunk"}`)
		}
	}))
	defer server.Close()
	g := NewGitHub("", server.Client())
	base, _ := url.Parse(server.URL + "/")
	g.Client.BaseURL = base
	s, err := g.Snapshot(context.Background(), "https://github.com/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	if tagPages != 2 || releasePages != 2 || len(s.Tags) != 2 || len(s.Releases) != 2 || !s.ReleasesComplete || s.DefaultBranch != "trunk" {
		b, _ := json.Marshal(s)
		t.Fatalf("pagination lost: %s (%d %d)", b, tagPages, releasePages)
	}
	commit, err := g.Commit(context.Background(), "https://github.com/example/repo", "abc")
	if err != nil || commit.SHA != "abc" || commit.Date == nil {
		t.Fatalf("commit %+v, %v", commit, err)
	}
	a, err := g.Activity(context.Background(), "https://github.com/example/repo", s, activityAsOf)
	if err != nil || a.StableReleases.Count == nil || *a.StableReleases.Count != 2 || a.Band != someActivity || releasePages != 2 {
		t.Fatalf("activity did not reuse paginated published releases: %+v %v", a, err)
	}
}

type countingProvider struct {
	snapshots, commits int
	releaseDate        *time.Time
}

func (p *countingProvider) Snapshot(context.Context, string) (Snapshot, error) {
	p.snapshots++
	return Snapshot{Tags: []Tag{{"v1.0.0", "abc"}}, Releases: []Release{{Tag: "v1.0.0", Date: p.releaseDate}}}, nil
}
func (p *countingProvider) Commit(context.Context, string, string) (Commit, error) {
	p.commits++
	return Commit{SHA: "abc"}, nil
}
func (*countingProvider) Activity(_ context.Context, _ string, s Snapshot, asOf time.Time) (report.Activity, error) {
	s.ReleasesComplete = true
	return SummarizeActivity(s, nil, asOf, asOf, true), nil
}

func TestPackageVersionIsNotSourceVersionAndDedup(t *testing.T) {
	p := &countingProvider{}
	r := report.New(time.Now())
	r.Projects = []report.Project{{Repository: "https://github.com/example/repo", Components: []report.Component{{
		Versions: []report.Version{{PackageVersion: "99.0.0", Ref: "v1.0.0"}, {PackageVersion: "98.0.0", Ref: "v1.0.0"}},
	}}}}
	Enrich(context.Background(), r, p, time.Now)
	if p.snapshots != 1 || p.commits != 1 || r.Projects[0].Components[0].Versions[0].Freshness.ResolvedTag != "v1.0.0" {
		t.Fatalf("source association/dedup failed %+v", r)
	}
}

func TestPublishedTagDoesNotNeedRedundantCommitRequest(t *testing.T) {
	date := time.Now().Add(-24 * time.Hour)
	p := &countingProvider{releaseDate: &date}
	r := report.New(time.Now())
	r.Projects = []report.Project{{Repository: "https://github.com/example/repo", Components: []report.Component{{
		Versions: []report.Version{{Ref: "v1.0.0"}, {Ref: "abc"}},
	}}}}
	Enrich(context.Background(), r, p, time.Now)
	if p.commits != 0 || r.Projects[0].Components[0].Versions[1].Freshness.AgeDays == nil {
		t.Fatal("snapshot release identity/age was not reused")
	}
}
