// Package upstream resolves source identities and release freshness.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	azgithub "github.com/Azure/dalec-build-defs/azcu/pkg/github"
	"github.com/google/go-github/v84/github"
	"github.com/jeremyrickard/dalec-project-health/internal/netx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

type Tag struct {
	Name, SHA string
}

type Release struct {
	ID       int64
	Tag      string
	Date     *time.Time
	Unstable bool
}

type Snapshot struct {
	Tags             []Tag
	Releases         []Release
	Archived         *bool
	DefaultBranch    string
	ReleasesComplete bool
}

type Commit struct {
	SHA  string
	Date *time.Time
}

type Provider interface {
	Snapshot(context.Context, string) (Snapshot, error)
	Commit(context.Context, string, string) (Commit, error)
	Activity(context.Context, string, Snapshot, time.Time) (report.Activity, error)
}

type GitHub struct {
	Client *github.Client
	Tags   azgithub.Interface
	Now    func() time.Time
}

func NewGitHub(token string, client *http.Client) *GitHub {
	if client == nil {
		client = netx.NewGitHub(netx.DefaultRequestTimeout)
	}
	gh := github.NewClient(client)
	if token != "" {
		gh = gh.WithAuthToken(token)
	}
	// The shared transport owns rate limits and retries. SDK preflight checks
	// otherwise return cached primary/secondary limits without reaching it.
	gh.DisableRateLimitCheck = true
	// Keep pagination and owner/repo parsing at the exported azcu boundary.
	// Client injection bypasses azcu's transport construction/retry wrapper.
	tags := azgithub.NewClient(azgithub.WithGitHubClient(&azgithub.GitHubClient{Client: gh}))
	return &GitHub{Client: gh, Tags: tags, Now: time.Now}
}

func ownerRepo(repository string) (string, string, error) {
	if !strings.HasPrefix(repository, "https://github.com/") {
		return "", "", fmt.Errorf("unsupported upstream host")
	}
	return azgithub.ExtractOwnerRepo(strings.TrimPrefix(repository, "https://github.com/"))
}

func (g *GitHub) Snapshot(ctx context.Context, repository string) (Snapshot, error) {
	ctx = netx.TraceFetches(ctx)
	var s Snapshot
	owner, repo, err := ownerRepo(repository)
	if err != nil {
		return s, err
	}
	info, _, err := g.Client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return s, providerError(ctx, "metadata", repository, "", 0, err)
	}
	archived := info.GetArchived()
	s.Archived = &archived
	s.DefaultBranch = info.GetDefaultBranch()
	opts := &github.ListOptions{PerPage: 100}
	for {
		releases, res, err := g.Client.Repositories.ListReleases(ctx, owner, repo, opts)
		if err != nil {
			return s, providerError(ctx, "releases", repository, "", max(opts.Page, 1), err)
		}
		for _, release := range releases {
			var date *time.Time
			if release.PublishedAt != nil && !release.PublishedAt.IsZero() {
				d := release.PublishedAt.Time
				date = &d
			}
			s.Releases = append(s.Releases, Release{ID: release.GetID(), Tag: release.GetTagName(), Date: date, Unstable: release.GetDraft() || release.GetPrerelease()})
		}
		next, err := nextGitHubPage(res, opts.Page)
		if err != nil {
			return s, err
		}
		if next == 0 {
			break
		}
		opts.Page = next
	}
	s.ReleasesComplete = true
	for tag, err := range g.Tags.GetTags(ctx, owner+"/"+repo) {
		if err != nil {
			return s, providerError(ctx, "tags pagination", repository, "", 0, err)
		}
		s.Tags = append(s.Tags, Tag{Name: tag.GetName(), SHA: tag.GetCommit().GetSHA()})
	}
	return s, nil
}

func (g *GitHub) Commit(ctx context.Context, repository, ref string) (Commit, error) {
	ctx = netx.TraceFetches(ctx)
	owner, repo, err := ownerRepo(repository)
	if err != nil {
		return Commit{}, err
	}
	c, _, err := g.Client.Repositories.GetCommit(ctx, owner, repo, ref, nil)
	if err != nil {
		return Commit{}, providerError(ctx, "commits", repository, ref, 0, err)
	}
	var date *time.Time
	if c.GetCommit().GetCommitter().Date != nil && !c.GetCommit().GetCommitter().Date.IsZero() {
		d := c.GetCommit().GetCommitter().Date.Time
		date = &d
	}
	return Commit{SHA: c.GetSHA(), Date: date}, nil
}

func providerError(ctx context.Context, operation, repository, ref string, page int, err error) error {
	err = netx.TracedError(ctx, err)
	var fetch *netx.FetchError
	if errors.As(err, &fetch) {
		return fetch
	}
	e := &netx.FetchError{
		Operation: netx.Operation(operation, repository, ref, page),
		Kind:      netx.FailureKind(ctx, err), Cause: err,
	}
	if ctx.Err() != nil {
		e.Cause = netx.ContextError(ctx)
		return e
	}
	var response *github.ErrorResponse
	var primary *github.RateLimitError
	var secondary *github.AbuseRateLimitError
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &shape):
		e.Kind = "invalid provider JSON response (not retried)"
	case errors.As(err, &primary):
		e.Kind, e.RetryAt = "primary rate limit", primary.Rate.Reset.Time
		if primary.Response != nil {
			e.Status = primary.Response.StatusCode
		}
	case errors.As(err, &secondary):
		e.Kind = "secondary rate limit"
		if secondary.Response != nil {
			e.Status = secondary.Response.StatusCode
		}
	case errors.As(err, &response) && response.Response != nil:
		e.Status = response.Response.StatusCode
		e.Kind = netx.HTTPFailure(e.Status)
	}
	return e
}
