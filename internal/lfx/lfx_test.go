package lfx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

func testProject() report.Project {
	return report.Project{Repository: "https://github.com/example/widget", Components: []report.Component{{
		Name: "widget", Versions: []report.Version{{Website: "https://widget.example.org"}},
	}}}
}

func TestIdentityVerification(t *testing.T) {
	tests := []struct {
		name string
		list []Project
		want int
	}{
		{"repo", []Project{{Slug: "widget", GitHub: "https://github.com/example/widget"}}, 1},
		{"org only", []Project{{Slug: "widget", GitHub: "https://github.com/example"}}, 0},
		{"corroborated website", []Project{{Slug: "widget", GitHub: "https://github.com/example", Website: "https://widget.example.org/"}}, 1},
		{"alias not guessed", []Project{{Slug: "unexpected-alias", GitHub: "https://github.com/example/widget"}}, 0},
		{"wrong repo", []Project{{Slug: "widget", GitHub: "https://github.com/else/widget", Website: "https://widget.example.org"}}, 0},
		{"ambiguous", []Project{{Slug: "widget", GitHub: "https://github.com/example/widget"}, {Slug: "example", GitHub: "https://github.com/example/widget"}}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(testProject(), tc.list); len(got) != tc.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestPublicContractAndNullScores(t *testing.T) {
	for _, score := range []string{"null", "0", "42"} {
		t.Run(score, func(t *testing.T) {
			maxScore := "65"
			if score == "null" {
				maxScore = "null"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/project" {
					if r.URL.Query().Get("slugs") != "widget" || r.URL.Query().Get("pageSize") != "1" || r.URL.Query().Get("healthBreakdown") != "true" {
						t.Error("unsupported or missing lookup parameters")
					}
					fmt.Fprint(w, `{"total":1,"data":[{"slug":"widget","name":"Widget","github":"https://github.com/example/widget"}]}`)
					return
				}
				if r.URL.Path != "/api/project/widget/insights" {
					t.Errorf("unexpected endpoint %s", r.URL)
				}
				fmt.Fprintf(w, `{"slug":"widget","type":"project","healthScoreV2":%s,"healthMaxScore":%s,"healthLabel":null,"coveredCategoryCount":2,"lifecycleLabel":"stable"}`, score, maxScore)
			}))
			defer server.Close()
			c := &Client{HTTP: server.Client(), BaseURL: server.URL}
			if _, err := c.List(context.Background(), []string{"widget"}); err != nil {
				t.Fatal(err)
			}
			i, err := c.Insights(context.Background(), "widget")
			if err != nil || i.Label != nil || i.Coverage == nil || *i.Coverage != 2 || (i.Score == nil) != (score == "null") {
				t.Fatalf("null / partial coverage lost %+v %v", i, err)
			}
			if i.MaxScore != nil && *i.MaxScore != 65 {
				t.Fatal("score denominator changed")
			}
		})
	}
}

func TestFailuresAndMissingCoverage(t *testing.T) {
	for _, status := range []int{404, 429, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		c := &Client{HTTP: server.Client(), BaseURL: server.URL}
		i, err := c.Insights(context.Background(), "widget")
		server.Close()
		if status == 404 && (err != nil || i.Slug != "") {
			t.Fatal("missing coverage should not be operational failure")
		}
		if status != 404 && err == nil {
			t.Fatal("operational failure hidden")
		}
	}
}

func TestMalformedAndPartialResponses(t *testing.T) {
	for _, tc := range []struct{ endpoint, body string }{
		{"list", `{"data":[]}`},
		{"list", `{"total":2,"data":[{"slug":"widget"}]}`},
		{"insights", `{"slug":"widget","type":"repo","healthScoreV2":42,"healthMaxScore":null}`},
		{"insights", `{"slug":"widget","type":"repo","healthScoreV2":80,"healthMaxScore":65}`},
		{"insights", `{"slug":"wrong","type":"repo"}`},
		{"insights", `not-json`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, tc.body)
		}))
		c := &Client{HTTP: server.Client(), BaseURL: server.URL}
		var err error
		if tc.endpoint == "list" {
			_, err = c.List(context.Background(), []string{"widget"})
		} else {
			_, err = c.Insights(context.Background(), "widget")
		}
		server.Close()
		if err == nil {
			t.Fatalf("malformed response accepted %s", tc.body)
		}
	}
}

func TestMissingInsightScopeIsExplicitlyUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"slug":"widget","name":"Widget","healthScoreV2":42,"healthMaxScore":65,"lifecycleLabel":"stable"}`)
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), BaseURL: server.URL}
	insight, err := c.Insights(context.Background(), "widget")
	if err != nil || insight.Type != "" || insight.Score == nil {
		t.Fatalf("missing optional scope should preserve available health: %+v %v", insight, err)
	}
}

type fakeProvider struct{ insights int }

func (*fakeProvider) List(context.Context, []string) ([]Project, error) {
	return []Project{{Slug: "widget", GitHub: "https://github.com/example/widget"}}, nil
}
func (p *fakeProvider) Insights(context.Context, string) (Insight, error) {
	p.insights++
	lifecycle := "active"
	return Insight{Slug: "widget", Type: "project", Lifecycle: &lifecycle}, nil
}

func TestLifecycleSeparateAndUnscored(t *testing.T) {
	r := report.New(time.Now())
	p := testProject()
	p.Lifecycle = report.Unknown
	r.Projects = []report.Project{p, p}
	provider := &fakeProvider{}
	Enrich(context.Background(), r, provider, time.Now)
	if provider.insights != 1 || r.Projects[0].Lifecycle != report.Unknown || r.Projects[0].LFX.Lifecycle != "active" || r.Projects[0].LFX.Score != nil {
		t.Fatalf("mixed support/activity or null semantics %+v", r.Projects[0])
	}
}
