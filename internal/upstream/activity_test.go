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

	"github.com/google/go-github/v84/github"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

var activityAsOf = time.Date(2026, 11, 17, 12, 0, 0, 123, time.FixedZone("test", 2*60*60))

func activityCommit(sha string, date time.Time, author int64) *github.RepositoryCommit {
	return &github.RepositoryCommit{
		SHA: github.Ptr(sha), Parents: []*github.Commit{{SHA: github.Ptr("parent")}},
		Author: &github.User{ID: github.Ptr(author), Login: github.Ptr("person"), Type: github.Ptr("User")},
		Commit: &github.Commit{
			Author:    &github.CommitAuthor{Name: github.Ptr("Person"), Email: github.Ptr("person@example.invalid"), Date: &github.Timestamp{Time: date}},
			Committer: &github.CommitAuthor{Date: &github.Timestamp{Time: date}},
		},
	}
}

func completeActivitySnapshot() Snapshot {
	return Snapshot{DefaultBranch: "trunk", ReleasesComplete: true}
}

func TestActivityExactUTCBoundariesAndPeriods(t *testing.T) {
	end, start := activityAsOf.UTC(), activityAsOf.UTC().Add(-activityWindow)
	commits := []*github.RepositoryCommit{activityCommit("before", start.Add(-time.Nanosecond), 1), activityCommit("at-end", end, 2)}
	for i := range 6 {
		commits = append(commits, activityCommit(fmt.Sprint(i), start.Add(time.Duration(i)*activityPeriod), 1))
	}
	commits = append(commits, activityCommit("last", end.Add(-time.Nanosecond), 1))
	a := SummarizeActivity(completeActivitySnapshot(), commits, activityAsOf, end.Add(time.Minute), true)
	if !a.WindowStart.Equal(start) || !a.WindowEnd.Equal(end) || a.WindowStart.Location() != time.UTC || a.WindowEnd.Location() != time.UTC {
		t.Fatal("window is not exact UTC [asOf-180d, asOf)")
	}
	if len(a.Periods) != 6 || a.DefaultBranchCommits.Count == nil || *a.DefaultBranchCommits.Count != 7 || a.Band != someActivity {
		t.Fatalf("boundaries counted incorrectly: %+v", a)
	}
	for i, period := range a.Periods {
		want := 1
		if i == 5 {
			want = 2
		}
		if period.End.Sub(period.Start) != activityPeriod || period.Commits.Count == nil || *period.Commits.Count != want {
			t.Fatalf("period %d: %+v", i, period)
		}
	}
	if a.Contributors.Count == nil || *a.Contributors.Count != 1 {
		t.Fatal("contributors were summed per period rather than deduplicated globally")
	}
}

func TestActivityBandsAndReleaseExclusions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		commits, periods int
		complete         bool
		band             string
	}{
		{"threshold", 12, 3, true, sustainedActivity},
		{"one short", 11, 3, true, someActivity},
		{"two periods", 12, 2, true, someActivity},
		{"zero complete", 0, 0, true, noActivity},
		{"zero incomplete", 0, 0, false, report.Unknown},
		{"busy incomplete", 100, 6, false, report.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commits := []*github.RepositoryCommit{}
			for i := 0; i < tc.commits; i++ {
				date := activityAsOf.Add(-time.Hour - time.Duration(i%tc.periods)*activityPeriod)
				commits = append(commits, activityCommit(fmt.Sprint(i), date, 1))
			}
			a := SummarizeActivity(completeActivitySnapshot(), commits, activityAsOf, activityAsOf, tc.complete)
			if a.Band != tc.band || a.WindowComplete != tc.complete {
				t.Fatalf("got %+v", a)
			}
			if tc.complete && tc.commits > 0 && *a.Contributors.Count != 1 {
				t.Fatal("single-maintainer breadth must not downgrade sustained activity")
			}
		})
	}
	start, end := activityAsOf.Add(-activityWindow), activityAsOf
	before, last := start.Add(-time.Nanosecond), end.Add(-time.Nanosecond)
	s := completeActivitySnapshot()
	s.Releases = []Release{
		{ID: 1, Tag: "release-final", Date: &start},
		{ID: 1, Tag: "release-final", Date: &start},
		{ID: 2, Tag: "v1.0.0", Date: &last},
		{ID: 3, Tag: "v2.0.0", Date: &end},
		{ID: 4, Tag: "v3.0.0", Date: &before},
		{ID: 5, Tag: "v4.0.0", Date: &last, Unstable: true},
		{ID: 6, Tag: "v5.0.0-rc.1", Date: &last},
		{ID: 7, Tag: "draft", Unstable: true},
		{ID: 8, Tag: "component/v6.0.0-rc.1", Date: &last},
	}
	a := SummarizeActivity(s, nil, activityAsOf, activityAsOf, true)
	if a.StableReleases.Count == nil || *a.StableReleases.Count != 2 || a.Band != someActivity {
		t.Fatalf("stable releases counted incorrectly %+v", a)
	}
	s.ReleasesComplete = false
	a = SummarizeActivity(s, nil, activityAsOf, activityAsOf, true)
	if a.Band != report.Unknown || a.StableReleases.Count != nil || a.StableReleases.Observed != 2 || a.DefaultBranchCommits.Count == nil {
		t.Fatal("partial release coverage presented as complete")
	}
	s = completeActivitySnapshot()
	s.Releases = []Release{{ID: 1, Tag: "missing-date"}}
	if a = SummarizeActivity(s, nil, activityAsOf, activityAsOf, true); a.Band != report.Unknown || a.StableReleases.Complete {
		t.Fatal("missing publication date silently discarded")
	}
}

func TestActivityAuthorsBotsMergesAndTimestampSemantics(t *testing.T) {
	date := activityAsOf.Add(-time.Hour)
	human := activityCommit("human", date, 1)
	duplicate := activityCommit("human", date, 1)
	second := activityCommit("second", date, 2)
	bot := activityCommit("provider-bot", date, 3)
	bot.Author.Type = github.Ptr("Bot")
	namedBot := activityCommit("named-bot", date, 4)
	namedBot.Author.Login = github.Ptr("service[bot]")
	emailBot := activityCommit("email-bot", date, 5)
	emailBot.Commit.Author.Email = github.Ptr("automation-bot@example.invalid")
	merge := activityCommit("merge", date, 6)
	merge.Parents = []*github.Commit{{}, {}}
	root := activityCommit("root", date, 1)
	root.Parents = []*github.Commit{}
	// API windows and buckets use committer timestamp, not author timestamp.
	human.Commit.Author.Date = &github.Timestamp{Time: activityAsOf.Add(-2 * activityWindow)}
	outside := activityCommit("author-only-in-window", activityAsOf.Add(-2*activityWindow), 7)
	outside.Commit.Author.Date = &github.Timestamp{Time: date}
	a := SummarizeActivity(completeActivitySnapshot(), []*github.RepositoryCommit{human, duplicate, second, bot, namedBot, emailBot, merge, root, outside}, activityAsOf, activityAsOf, true)
	if a.DefaultBranchCommits.Count == nil || *a.DefaultBranchCommits.Count != 3 || *a.Contributors.Count != 2 {
		t.Fatalf("author/bot/merge/date semantics incorrect %+v", a)
	}
	body, _ := json.Marshal(a)
	for _, secret := range []string{"person@example.invalid", "person\"", "provider-bot", `"author"`, `"login"`, `"email"`} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("author/commit identities leaked into aggregate report: %s", secret)
		}
	}
	unlinked := activityCommit("unlinked", date, 0)
	unlinked.Author = nil
	a = SummarizeActivity(completeActivitySnapshot(), []*github.RepositoryCommit{human, unlinked}, activityAsOf, activityAsOf, true)
	if a.Contributors.Count != nil || a.Contributors.Observed != 1 || *a.DefaultBranchCommits.Count != 2 || a.Band != someActivity {
		t.Fatal("unlinked authors silently undercounted or wrongly changed independent activity band")
	}
	for _, kind := range []string{"author", "empty author", "parents", "date", "identity", "record"} {
		c := activityCommit("bad", date, 1)
		switch kind {
		case "author":
			c.Author, c.Commit.Author = nil, nil
		case "empty author":
			c.Author, c.Commit.Author = &github.User{}, &github.CommitAuthor{}
		case "parents":
			c.Parents = nil
		case "date":
			c.Commit.Committer.Date = nil
		case "identity":
			c.SHA = nil
		case "record":
			c = nil
		}
		a = SummarizeActivity(completeActivitySnapshot(), []*github.RepositoryCommit{human, c}, activityAsOf, activityAsOf, true)
		if a.Band != report.Unknown || a.DefaultBranchCommits.Count != nil || a.Contributors.Count != nil || a.DefaultBranchCommits.Observed != 1 {
			t.Fatalf("missing %s metadata was treated as complete: %+v", kind, a)
		}
	}
}

func TestActivityDefaultBranchPaginationAndPartialFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			commitsRequests := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/branches/trunk"):
					fmt.Fprint(w, `{"name":"trunk","commit":{"sha":"pinned-head"}}`)
				case strings.HasSuffix(r.URL.Path, "/commits"):
					commitsRequests++
					q := r.URL.Query()
					if q.Get("sha") != "pinned-head" || q.Get("per_page") != "100" {
						t.Error("commit request did not explicitly pin the default branch head")
					}
					since, _ := time.Parse(time.RFC3339, q.Get("since"))
					until, _ := time.Parse(time.RFC3339, q.Get("until"))
					if !since.Before(activityAsOf.UTC().Add(-activityWindow)) || !until.After(activityAsOf.UTC()) || until.Sub(activityAsOf.UTC()) > time.Second {
						t.Errorf("incorrect query bounds %v", q)
					}
					if q.Get("page") == "" {
						w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/commits?page=2>; rel=\"next\"", server.URL))
					} else if fail {
						w.WriteHeader(429)
						return
					}
					body, _ := json.Marshal([]*github.RepositoryCommit{activityCommit(fmt.Sprint(commitsRequests), activityAsOf.Add(-time.Hour), 1)})
					w.Write(body)
				default:
					t.Errorf("unexpected activity API request %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			g := NewGitHub("", server.Client())
			g.Client.BaseURL, _ = url.Parse(server.URL + "/")
			g.Now = func() time.Time { return activityAsOf.Add(time.Minute) }
			a, err := g.Activity(context.Background(), "https://github.com/example/repo", completeActivitySnapshot(), activityAsOf)
			if commitsRequests != 2 || a.DefaultBranch != "trunk" || a.HeadSHA != "pinned-head" || a.RetrievedAt == nil {
				t.Fatalf("provenance or pagination incorrect: %+v", a)
			}
			if fail {
				if err == nil || a.WindowComplete || a.Band != report.Unknown || a.DefaultBranchCommits.Observed != 1 || a.DefaultBranchCommits.Count != nil || a.StableReleases.Count == nil {
					t.Fatalf("partial results hidden: %+v %v", a, err)
				}
			} else if err != nil || !a.WindowComplete || *a.DefaultBranchCommits.Count != 2 {
				t.Fatalf("complete observation failed %+v %v", a, err)
			}
		})
	}
}

type eligibilityProvider struct {
	countingProvider
	activityCalls int
}

func (p *eligibilityProvider) Activity(_ context.Context, _ string, s Snapshot, asOf time.Time) (report.Activity, error) {
	p.activityCalls++
	s.ReleasesComplete = true
	return SummarizeActivity(s, nil, asOf, asOf, true), nil
}

func TestActivityLFXPrecedenceAndEligibility(t *testing.T) {
	score, maxScore := 0.0, 65.0
	for _, tc := range []struct {
		name     string
		health   report.Health
		eligible bool
	}{
		{"absent", report.Health{Status: report.Unknown}, true},
		{"failed", report.Health{Status: "Failed"}, true},
		{"unscored", report.Health{Status: "Available", Lifecycle: "active"}, true},
		{"missing lifecycle", report.Health{Status: "Available", Score: &score, MaxScore: &maxScore, Lifecycle: report.Unknown}, true},
		{"complete zero score", report.Health{Status: "Available", Score: &score, MaxScore: &maxScore, Lifecycle: "active", Scope: report.Unknown}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report.New(activityAsOf)
			r.Projects = []report.Project{{Repository: "https://github.com/example/repo", Lifecycle: report.Unknown, LFX: tc.health}}
			p := &eligibilityProvider{}
			Enrich(context.Background(), r, p, func() time.Time { return activityAsOf })
			if (p.activityCalls == 1) != tc.eligible || (r.Projects[0].Activity != nil) != tc.eligible {
				t.Fatal("activity fetched for complete LFX or omitted for eligible fallback")
			}
			if r.Projects[0].Lifecycle != report.Unknown || r.Projects[0].LFX.Lifecycle != tc.health.Lifecycle || r.Projects[0].LFX.Score != tc.health.Score {
				t.Fatal("inferred activity overwrote support or native LFX evidence")
			}
		})
	}
}

func TestActivityMissingBranchAndCancellation(t *testing.T) {
	g := NewGitHub("", &http.Client{Transport: rejectingTransport{t}})
	a, err := g.Activity(context.Background(), "https://github.com/example/repo", Snapshot{ReleasesComplete: true}, activityAsOf)
	if err == nil || a.Band != report.Unknown || a.DefaultBranchCommits.Count != nil || a.StableReleases.Count == nil {
		t.Fatal("missing branch was treated as a complete zero-commit window")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, err = g.Activity(ctx, "https://github.com/example/repo", completeActivitySnapshot(), activityAsOf)
	if err == nil || a.WindowComplete || a.Band != report.Unknown {
		t.Fatal("cancelled fetch classified activity")
	}
}

type rejectingTransport struct{ t *testing.T }

func (r rejectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Context().Err() == nil {
		r.t.Error("unexpected activity request")
	}
	return nil, context.Canceled
}

func TestActivityCoverageFailuresNeverClaimInactivity(t *testing.T) {
	for _, mode := range []string{"inaccessible branch", "timeout", "pagination did not advance", "malformed next page"} {
		t.Run(mode, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/branches/trunk") {
					switch mode {
					case "inaccessible branch":
						w.WriteHeader(http.StatusNotFound)
						return
					case "timeout":
						<-r.Context().Done()
						return
					}
					fmt.Fprint(w, `{"commit":{"sha":"pinned"}}`)
					return
				}
				page := "1"
				if mode == "malformed next page" {
					page = "invalid"
				}
				w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/commits?page=%s>; rel=\"next\"", server.URL, page))
				body, _ := json.Marshal([]*github.RepositoryCommit{activityCommit("one", activityAsOf.Add(-time.Hour), 1)})
				w.Write(body)
			}))
			defer server.Close()
			g := NewGitHub("", server.Client())
			g.Client.BaseURL, _ = url.Parse(server.URL + "/")
			budget := 2 * time.Second
			if mode == "timeout" {
				budget = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			a, err := g.Activity(ctx, "https://github.com/example/repo", completeActivitySnapshot(), activityAsOf)
			if err == nil || a.Band != report.Unknown || a.WindowComplete || a.DefaultBranchCommits.Count != nil || a.StableReleases.Count == nil {
				t.Fatalf("coverage failure looked complete: %+v %v", a, err)
			}
			if mode == "pagination did not advance" && a.DefaultBranchCommits.Observed != 1 {
				t.Fatal("known partial commits were discarded")
			}
		})
	}
}

func TestActivitySurvivesFreshnessTagFailure(t *testing.T) {
	releaseRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags"):
			w.WriteHeader(500)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			releaseRequests++
			fmt.Fprint(w, `[{"id":1,"tag_name":"v1.0.0","published_at":"2026-10-01T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/branches/trunk"):
			fmt.Fprint(w, `{"commit":{"sha":"pinned"}}`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		default:
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		}
	}))
	defer server.Close()
	g := NewGitHub("", server.Client())
	g.Client.BaseURL, _ = url.Parse(server.URL + "/")
	r := report.New(activityAsOf)
	r.Projects = []report.Project{{Repository: "https://github.com/example/repo"}}
	Enrich(context.Background(), r, g, func() time.Time { return activityAsOf })
	a := r.Projects[0].Activity
	if !r.Partial || a == nil || a.Band != someActivity || !a.WindowComplete || *a.StableReleases.Count != 1 || releaseRequests != 1 {
		t.Fatalf("freshness failure lost reusable activity evidence: %+v", r)
	}
}

func TestActivityUnsupportedAndUnvisitedProjectsStayExplicit(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		r := report.New(activityAsOf)
		r.Projects = []report.Project{{Repository: "https://gitlab.com/example/repo"}, {}}
		if canceled {
			r.Projects = append(r.Projects, report.Project{Repository: "https://github.com/example/repo"})
		}
		p := &eligibilityProvider{}
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		Enrich(ctx, r, p, func() time.Time { return activityAsOf })
		cancel()
		if p.snapshots != 0 || p.activityCalls != 0 {
			t.Fatal("unsupported or cancelled project caused requests")
		}
		for _, project := range r.Projects {
			a := project.Activity
			if a == nil || a.Band != report.Unknown || a.DefaultBranchCommits.Count != nil || a.RetrievedAt != nil || len(a.Periods) != 6 {
				t.Fatal("missing activity appeared complete or disappeared")
			}
		}
	}
}
