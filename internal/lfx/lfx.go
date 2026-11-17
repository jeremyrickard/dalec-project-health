// Package lfx matches supported public LFX identity fields conservatively.
package lfx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/inventory"
	"github.com/jeremyrickard/dalec-project-health/internal/netx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

type Project struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	GitHub  string `json:"github"`
	Website string `json:"website"`
}

type Insight struct {
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	RepoURL   string   `json:"repoUrl"`
	Lifecycle *string  `json:"lifecycleLabel"`
	Score     *float64 `json:"healthScoreV2"`
	MaxScore  *float64 `json:"healthMaxScore"`
	Label     *string  `json:"healthLabel"`
	Coverage  *int     `json:"coveredCategoryCount"`
}

type Provider interface {
	List(context.Context, []string) ([]Project, error)
	Insights(context.Context, string) (Insight, error)
}

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func New() *Client {
	return &Client{HTTP: netx.New(310 * time.Millisecond), BaseURL: "https://insights.linuxfoundation.org"}
}

func (c *Client) get(ctx context.Context, endpoint string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("invalid LFX endpoint configuration")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("LFX request failed (network, rate limit, or cancellation)")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if res.StatusCode != http.StatusOK {
		return false, fmt.Errorf("LFX HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return false, fmt.Errorf("LFX returned invalid JSON")
	}
	return true, nil
}

func (c *Client) List(ctx context.Context, slugs []string) ([]Project, error) {
	q := url.Values{"slugs": {strings.Join(slugs, ",")}, "pageSize": {fmt.Sprint(len(slugs))}, "healthBreakdown": {"true"}}
	var result struct {
		Data  []Project `json:"data"`
		Total *int      `json:"total"`
	}
	found, err := c.get(ctx, "/api/project?"+q.Encode(), &result)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("LFX project-list endpoint unavailable")
	}
	if result.Data == nil || result.Total == nil || *result.Total != len(result.Data) {
		return nil, fmt.Errorf("LFX project list is malformed or incomplete")
	}
	for _, p := range result.Data {
		if !slices.Contains(slugs, p.Slug) {
			return nil, fmt.Errorf("LFX returned an unrequested project identity")
		}
	}
	return result.Data, nil
}

func (c *Client) Insights(ctx context.Context, slug string) (Insight, error) {
	var insight Insight
	found, err := c.get(ctx, "/api/project/"+url.PathEscape(slug)+"/insights", &insight)
	if err != nil {
		return insight, err
	}
	if !found {
		return insight, nil
	}
	if insight.Slug != slug || (insight.Type != "" && insight.Type != "project" && insight.Type != "repo") {
		return Insight{}, fmt.Errorf("LFX insight identity is malformed")
	}
	if (insight.Score == nil) != (insight.MaxScore == nil) || insight.Score != nil &&
		(*insight.Score < 0 || *insight.Score > *insight.MaxScore || !slices.Contains([]float64{60, 65, 75, 100}, *insight.MaxScore)) {
		return Insight{}, fmt.Errorf("LFX health score range is invalid")
	}
	if insight.Coverage != nil && (*insight.Coverage < 0 || *insight.Coverage > 3) {
		return Insight{}, fmt.Errorf("LFX category coverage is invalid")
	}
	return insight, nil
}

var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func candidateSlugs(p report.Project) []string {
	if p.Repository == "" {
		return nil
	}
	u, err := url.Parse(p.Repository)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	candidates := []string{path.Base(u.Path)}
	if len(parts) > 1 {
		candidates = append(candidates, parts[0])
	}
	for _, c := range p.Components {
		candidates = append(candidates, c.Name)
	}
	result := []string{}
	for _, c := range candidates {
		c = strings.ToLower(c)
		if validSlug.MatchString(c) {
			result = append(result, c)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func Match(p report.Project, candidates []Project) []Project {
	result := []Project{}
	for _, c := range candidates {
		if !slices.Contains(candidateSlugs(p), c.Slug) {
			continue
		}
		repo := inventory.RepositoryURL(c.GitHub)
		if repo == p.Repository && repo != "" {
			result = append(result, c)
			continue
		}
		// An org-only GitHub link is insufficient by itself. A corroborating
		// exact project website can identify an umbrella project assessment.
		website := inventory.WebsiteURL(c.Website)
		if website == "" || strings.HasPrefix(website, "https://github.com/") || strings.HasPrefix(website, "http://github.com/") {
			continue
		}
		gitURL, _ := url.Parse(c.GitHub)
		if gitURL != nil && strings.EqualFold(gitURL.Hostname(), "github.com") && len(strings.Split(strings.Trim(gitURL.Path, "/"), "/")) == 2 && repo != p.Repository {
			continue
		}
		matched := false
		for _, component := range p.Components {
			for _, v := range component.Versions {
				matched = matched || website == v.Website
			}
		}
		if matched {
			result = append(result, c)
		}
	}
	slices.SortFunc(result, func(a, b Project) int { return strings.Compare(a.Slug, b.Slug) })
	return slices.CompactFunc(result, func(a, b Project) bool { return a.Slug == b.Slug })
}

func Enrich(ctx context.Context, r *report.Report, provider Provider, now func() time.Time) {
	slugs := []string{}
	for _, p := range r.Projects {
		slugs = append(slugs, candidateSlugs(p)...)
	}
	slices.Sort(slugs)
	slugs = slices.Compact(slugs)
	candidates := []Project{}
	failedSlugs := map[string]bool{}
	for start := 0; start < len(slugs); start += 100 {
		batch := slugs[start:min(start+100, len(slugs))]
		projects, err := provider.List(ctx, batch)
		if err != nil {
			r.Failure("LFX", "candidate lookup", err)
			for _, slug := range batch {
				failedSlugs[slug] = true
			}
		} else {
			candidates = append(candidates, projects...)
		}
		if ctx.Err() != nil {
			for _, slug := range slugs[start:] {
				failedSlugs[slug] = true
			}
			break
		}
	}
	type result struct {
		insight Insight
		err     error
	}
	cache := map[string]result{}
	for pi := range r.Projects {
		p := &r.Projects[pi]
		if p.Repository == "" {
			p.LFX.Reason = "No unambiguous upstream identity to verify"
			continue
		}
		failed := false
		for _, slug := range candidateSlugs(*p) {
			failed = failed || failedSlugs[slug]
		}
		if failed {
			p.LFX.Status, p.LFX.Reason = "Failed", "LFX candidate lookup incomplete"
			continue
		}
		matches := Match(*p, candidates)
		if len(matches) != 1 {
			p.LFX.Reason = "No unique verified LFX identity; aliases and org-only links are not proof"
			continue
		}
		match := matches[0]
		cached, ok := cache[match.Slug]
		if !ok {
			cached.insight, cached.err = provider.Insights(ctx, match.Slug)
			cache[match.Slug] = cached
			if cached.err != nil {
				r.Failure("LFX", match.Slug, cached.err)
			}
		}
		if cached.err != nil {
			p.LFX.Status, p.LFX.Reason = "Failed", cached.err.Error()
			continue
		}
		i := cached.insight
		if i.Slug == "" {
			p.LFX.Reason = "Verified project has no public insights coverage"
			continue
		}
		if i.Type == "repo" && inventory.RepositoryURL(i.RepoURL) != p.Repository {
			p.LFX.Reason = "LFX repository assessment does not identify selected repository"
			continue
		}
		lifecycle := report.Unknown
		if i.Lifecycle != nil && slices.Contains([]string{"active", "stable", "declining", "inert", "abandoned", "archived"}, *i.Lifecycle) {
			lifecycle = *i.Lifecycle
		}
		retrieved := now().UTC()
		scope := i.Type
		if scope == "" {
			scope = report.Unknown
		}
		p.LFX = report.Health{Status: "Available", Slug: i.Slug, Name: i.Name, Lifecycle: lifecycle,
			Score: i.Score, MaxScore: i.MaxScore, Label: i.Label, Coverage: i.Coverage, Scope: scope,
			IdentityGitHub: inventory.WebsiteURL(match.GitHub), IdentityWebsite: inventory.WebsiteURL(match.Website),
			EvidenceURL: "https://insights.linuxfoundation.org/projects/" + url.PathEscape(i.Slug), RetrievedAt: &retrieved,
			Reason: "Verified supported identity fields; project assessments may cover multiple repositories"}
		if i.Score == nil {
			p.LFX.Reason += "; health score is unscored (not zero)"
		}
		if scope == report.Unknown {
			p.LFX.Reason += "; provider did not supply assessment scope"
		}
	}
}
