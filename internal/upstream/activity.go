package upstream

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/go-github/v84/github"
	"github.com/jeremyrickard/dalec-project-health/internal/netx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

const (
	activityPeriod    = 30 * 24 * time.Hour
	activityWindow    = 6 * activityPeriod
	sustainedActivity = "Sustained activity"
	someActivity      = "Some activity"
	noActivity        = "No observed activity"
)

const activityMethodology = "github-rolling-activity/v1"
const activityMethod = "Exact UTC [asOf-180 days, asOf) window in six consecutive rolling 30-day periods, not calendar months. Sustained activity: at least 12 qualifying commits across at least 3 periods; Some activity: any qualifying commit or stable release; No observed activity: complete commit/release counts are both zero. Bands require complete commit and release coverage. Commits are non-merge, non-bot commits reachable from the pinned default-branch head, bucketed by committer timestamp, not author date or PR merge date. Direct pushes and squash/rebase paths are included; these are not merged PR counts. Contributors are distinct linked GitHub author accounts on qualifying commits, deduplicated transiently by provider ID, not people; unlinked/missing IDs make breadth Unknown. Bots are excluded by provider Bot type or author login/name/email local-part ending [bot] or -bot; unmarked bots can remain and human names can match. Stable releases exclude drafts, provider prereleases, and recognizable semver prerelease tags; tag-only projects can have zero GitHub releases. No author identities are saved. Activity is not a health score, support promise, maintenance assertion, or EOL determination."

func activityTrigger(p report.Project) string {
	h := p.LFX
	if h.Status != "Available" {
		return "No verified usable LFX assessment"
	}
	reasons := []string{}
	if h.Lifecycle == "" || h.Lifecycle == report.Unknown {
		reasons = append(reasons, "LFX native activity lifecycle is missing")
	}
	if h.Score == nil || h.MaxScore == nil || *h.MaxScore <= 0 || *h.Score < 0 || *h.Score > *h.MaxScore {
		reasons = append(reasons, "LFX health score is missing or unscored")
	}
	return strings.Join(reasons, "; ")
}

func newActivity(asOf time.Time) report.Activity {
	end := asOf.UTC()
	a := report.Activity{
		Source: "GitHub inferred activity", Status: "Not attempted", Band: report.Unknown,
		Reason: "Activity enrichment not attempted", MethodologyVersion: activityMethodology,
		Methodology: activityMethod, WindowStart: end.Add(-activityWindow), WindowEnd: end,
		Periods: []report.ActivityPeriod{}, Issues: []string{},
	}
	for i := range 6 {
		start := a.WindowStart.Add(time.Duration(i) * activityPeriod)
		a.Periods = append(a.Periods, report.ActivityPeriod{Start: start, End: start.Add(activityPeriod)})
	}
	return a
}

type activityCollector struct {
	result                 report.Activity
	commitMetadataComplete bool
	authorIDsComplete      bool
	releasesComplete       bool
	authors                map[int64]bool
	commits                map[string]bool
}

func newActivityCollector(s Snapshot, asOf time.Time) *activityCollector {
	c := &activityCollector{
		result: newActivity(asOf), commitMetadataComplete: true, authorIDsComplete: true,
		releasesComplete: s.ReleasesComplete, authors: map[int64]bool{}, commits: map[string]bool{},
	}
	c.result.DefaultBranch = s.DefaultBranch
	if !s.ReleasesComplete {
		c.issue("Release pagination is incomplete; observed release count is only a lower bound")
	}
	seen := map[string]bool{}
	for _, release := range s.Releases {
		_, version := splitTag(release.Tag)
		if release.Unstable || version != nil && version.Prerelease() != "" {
			continue
		}
		if release.Date == nil || release.Date.IsZero() {
			c.releasesComplete = false
			c.issue("A stable release has no published timestamp; full window coverage is Unknown")
			continue
		}
		if release.Date.Before(c.result.WindowStart) || !release.Date.Before(c.result.WindowEnd) {
			continue
		}
		key := fmt.Sprintf("id:%d", release.ID)
		if release.ID == 0 {
			if release.Tag == "" {
				c.releasesComplete = false
				c.issue("A release lacks a deduplicatable identity")
				continue
			}
			key = release.Tag + "@" + release.Date.UTC().Format(time.RFC3339Nano)
		}
		if !seen[key] {
			c.result.StableReleases.Observed++
			seen[key] = true
		}
	}
	return c
}

func (c *activityCollector) issue(message string) {
	if !slices.Contains(c.result.Issues, message) {
		c.result.Issues = append(c.result.Issues, message)
	}
}

func botName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[:at]
	}
	return strings.HasSuffix(value, "[bot]") || strings.HasSuffix(value, "-bot")
}

func isBot(commit *github.RepositoryCommit) bool {
	author := commit.GetAuthor()
	raw := commit.GetCommit().GetAuthor()
	return strings.EqualFold(author.GetType(), "Bot") || botName(author.GetLogin()) || botName(raw.GetName()) || botName(raw.GetEmail())
}

func (c *activityCollector) add(commit *github.RepositoryCommit) {
	if commit == nil {
		c.commitMetadataComplete = false
		c.issue("A commit record is missing")
		return
	}
	if len(commit.Parents) > 1 || isBot(commit) {
		return
	}
	date := commit.GetCommit().GetCommitter().GetDate()
	if date.IsZero() {
		c.commitMetadataComplete = false
		c.issue("A potentially qualifying commit has no committer timestamp")
		return
	}
	if date.Before(c.result.WindowStart) || !date.Before(c.result.WindowEnd) {
		return
	}
	author, raw := commit.GetAuthor(), commit.GetCommit().GetAuthor()
	hasAuthor := author.GetID() > 0 || author.GetLogin() != "" || author.GetType() != "" || raw.GetName() != "" || raw.GetEmail() != ""
	if commit.GetSHA() == "" || commit.Parents == nil || !hasAuthor {
		c.commitMetadataComplete = false
		c.issue("Commit identity, parent, or author metadata is missing; qualifying commit counts are Unknown")
		return
	}
	if c.commits[commit.GetSHA()] {
		return
	}
	c.commits[commit.GetSHA()] = true
	c.result.DefaultBranchCommits.Observed++
	period := int(date.Sub(c.result.WindowStart) / activityPeriod)
	c.result.Periods[period].Commits.Observed++
	if id := commit.GetAuthor().GetID(); id > 0 {
		c.authors[id] = true
	} else {
		c.authorIDsComplete = false
		c.issue("Some qualifying commit authors lack linked provider IDs; contributor breadth is Unknown")
	}
}

func metric(observed int, complete bool) report.ActivityCount {
	m := report.ActivityCount{Observed: observed, Complete: complete}
	if complete {
		m.Count = &observed
	}
	return m
}

func (c *activityCollector) finish(retrieved time.Time, commitsComplete bool) report.Activity {
	a := c.result
	t := retrieved.UTC()
	a.RetrievedAt = &t
	complete := commitsComplete && c.commitMetadataComplete
	a.StableReleases = metric(a.StableReleases.Observed, c.releasesComplete)
	a.DefaultBranchCommits = metric(a.DefaultBranchCommits.Observed, complete)
	a.Contributors = metric(len(c.authors), complete && c.authorIDsComplete)
	activePeriods := 0
	for i := range a.Periods {
		a.Periods[i].Commits = metric(a.Periods[i].Commits.Observed, complete)
		if a.Periods[i].Commits.Observed > 0 {
			activePeriods++
		}
	}
	a.WindowComplete = complete && c.releasesComplete
	a.Status, a.Reason = "Partial", "Insufficient evidence: commit/release coverage is incomplete; observed values are lower bounds"
	if a.WindowComplete {
		a.Status = "Available"
		switch {
		case a.DefaultBranchCommits.Observed >= 12 && activePeriods >= 3:
			a.Band, a.Reason = sustainedActivity, "At least 12 qualifying default-branch commits across at least 3 rolling 30-day periods"
		case a.DefaultBranchCommits.Observed > 0 || a.StableReleases.Observed > 0:
			a.Band, a.Reason = someActivity, "Some qualifying default-branch commits or published stable GitHub releases were observed"
		default:
			a.Band, a.Reason = noActivity, "Complete window: zero qualifying default-branch commits and zero published stable GitHub releases"
		}
	}
	return a
}

// SummarizeActivity is also useful to injected providers with already fetched
// evidence. Author data is consumed transiently and never enters the report.
func SummarizeActivity(s Snapshot, commits []*github.RepositoryCommit, asOf, retrieved time.Time, commitsComplete bool) report.Activity {
	c := newActivityCollector(s, asOf)
	for _, commit := range commits {
		c.add(commit)
	}
	return c.finish(retrieved, commitsComplete)
}

func (g *GitHub) Activity(ctx context.Context, repository string, s Snapshot, asOf time.Time) (report.Activity, error) {
	ctx = netx.TraceFetches(ctx)
	c := newActivityCollector(s, asOf)
	c.result.ReleaseEvidenceURL = repository + "/releases"
	fail := func(err error) (report.Activity, error) {
		c.issue(err.Error())
		return c.finish(g.Now(), false), err
	}
	owner, repo, err := ownerRepo(repository)
	if err != nil {
		return fail(err)
	}
	if s.DefaultBranch == "" {
		return fail(fmt.Errorf("GitHub default branch is unavailable"))
	}
	branch, _, err := g.Client.Repositories.GetBranch(ctx, owner, repo, s.DefaultBranch, 0)
	if err != nil {
		return fail(providerError(ctx, "default branch", repository, s.DefaultBranch, 0, err))
	}
	head := branch.GetCommit().GetSHA()
	if head == "" {
		return fail(fmt.Errorf("GitHub default branch head is unavailable"))
	}
	c.result.HeadSHA = head
	c.result.EvidenceURL = repository + "/commits/" + url.PathEscape(head)
	// Query a superset at second precision; enforce the exact half-open window locally.
	opts := &github.CommitsListOptions{
		SHA: head, Since: c.result.WindowStart.Truncate(time.Second).Add(-time.Second),
		Until: c.result.WindowEnd.Truncate(time.Second).Add(time.Second), ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		commits, res, err := g.Client.Repositories.ListCommits(ctx, owner, repo, opts)
		if err != nil {
			return fail(providerError(ctx, "activity commits", repository, head, max(opts.Page, 1), err))
		}
		for _, commit := range commits {
			c.add(commit)
		}
		next, err := nextGitHubPage(res, opts.Page)
		if err != nil {
			return fail(err)
		}
		if next == 0 {
			break
		}
		opts.Page = next
	}
	a := c.finish(g.Now(), true)
	if !a.WindowComplete {
		return a, fmt.Errorf("GitHub activity metadata does not establish a complete observation window")
	}
	return a, nil
}
