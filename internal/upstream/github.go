// Package upstream resolves source identities and release freshness.
package upstream

import (
	"context"
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
		client = netx.New(110 * time.Millisecond)
	}
	gh := github.NewClient(client)
	if token != "" {
		gh = gh.WithAuthToken(token)
	}
	// Keep pagination and owner/repo parsing at the exported azcu boundary.
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
	var s Snapshot
	owner, repo, err := ownerRepo(repository)
	if err != nil {
		return s, err
	}
	info, _, err := g.Client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return s, providerError(err)
	}
	archived := info.GetArchived()
	s.Archived = &archived
	s.DefaultBranch = info.GetDefaultBranch()
	opts := &github.ListOptions{PerPage: 100}
	for {
		releases, res, err := g.Client.Repositories.ListReleases(ctx, owner, repo, opts)
		if err != nil {
			return s, providerError(err)
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
			return s, providerError(err)
		}
		s.Tags = append(s.Tags, Tag{Name: tag.GetName(), SHA: tag.GetCommit().GetSHA()})
	}
	return s, nil
}

func (g *GitHub) Commit(ctx context.Context, repository, ref string) (Commit, error) {
	owner, repo, err := ownerRepo(repository)
	if err != nil {
		return Commit{}, err
	}
	c, _, err := g.Client.Repositories.GetCommit(ctx, owner, repo, ref, nil)
	if err != nil {
		return Commit{}, providerError(err)
	}
	var date *time.Time
	if c.GetCommit().GetCommitter().Date != nil && !c.GetCommit().GetCommitter().Date.IsZero() {
		d := c.GetCommit().GetCommitter().Date.Time
		date = &d
	}
	return Commit{SHA: c.GetSHA(), Date: date}, nil
}

func providerError(err error) error {
	// Provider errors can contain credential-bearing request URLs or bodies.
	// Retain the actionable HTTP status without copying untrusted error text.
	if e, ok := err.(*github.ErrorResponse); ok && e.Response != nil {
		return fmt.Errorf("GitHub HTTP %d", e.Response.StatusCode)
	}
	return fmt.Errorf("GitHub request failed (network, rate limit, or cancellation)")
}
