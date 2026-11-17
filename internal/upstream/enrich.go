package upstream

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

var versionTag = regexp.MustCompile(`^(.*?)(v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$`)
var wordPrefix = regexp.MustCompile(`^[A-Za-z]+$`)

func splitTag(tag string) (string, *semver.Version) {
	m := versionTag.FindStringSubmatch(tag)
	if m == nil {
		return "", nil
	}
	if prefix := m[1]; prefix != "" && !wordPrefix.MatchString(prefix) && !strings.ContainsAny(prefix[len(prefix)-1:], "/_-@") {
		return "", nil
	}
	v, err := semver.StrictNewVersion(strings.TrimPrefix(m[2], "v"))
	if err != nil {
		return "", nil
	}
	return m[1], v
}

func Enrich(ctx context.Context, r *report.Report, provider Provider, now func() time.Time) {
	for pi := range r.Projects {
		if ctx.Err() != nil {
			return
		}
		p := &r.Projects[pi]
		if p.Repository == "" {
			continue
		}
		if !strings.HasPrefix(p.Repository, "https://github.com/") {
			for ci := range p.Components {
				for vi := range p.Components[ci].Versions {
					p.Components[ci].Versions[vi].Freshness.Reason = "Enrichment unavailable for non-GitHub upstream"
				}
			}
			continue
		}
		projectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		snapshot, err := provider.Snapshot(projectCtx, p.Repository)
		if snapshot.Archived != nil {
			p.GitHub = &report.RepositoryState{Archived: *snapshot.Archived, RetrievedAt: now().UTC(), EvidenceURL: p.Repository}
		}
		if err != nil {
			cancel()
			r.Failure("GitHub", p.Repository, err)
			for ci := range p.Components {
				for vi := range p.Components[ci].Versions {
					v := &p.Components[ci].Versions[vi]
					v.Freshness.Status, v.Freshness.Reason = "Failed", err.Error()
				}
			}
			if ctx.Err() != nil {
				return
			}
			continue
		}
		commits := map[string]Commit{}
		commitErrors := map[string]error{}
		budgetReported := false
		for ci := range p.Components {
			for vi := range p.Components[ci].Versions {
				v := &p.Components[ci].Versions[vi]
				if err := projectCtx.Err(); err != nil {
					v.Freshness.Reason = "Enrichment not attempted: upstream request budget exhausted or cancelled"
					if !budgetReported {
						r.Failure("GitHub", p.Repository, err)
						budgetReported = true
					}
					continue
				}
				if v.Ref == "" || strings.Contains(v.Ref, "$") {
					v.Freshness.Reason = "Source ref is missing, unresolved, or ambiguous"
					continue
				}
				ref := strings.TrimPrefix(v.Ref, "refs/tags/")
				commit, ok := commits[ref]
				err := commitErrors[ref]
				if !ok && err == nil {
					// A tag's SHA plus its published release already supplies
					// identity and age. Avoid a redundant commit API request.
					for _, tag := range snapshot.Tags {
						if tag.Name == ref || tag.SHA == ref {
							commit.SHA = tag.SHA
							if tag.Name == ref {
								break
							}
						}
					}
					known := Resolve(snapshot, ref, commit, p.Repository, now())
					if commit.SHA == "" || known.Date == nil {
						commit, err = provider.Commit(projectCtx, p.Repository, v.Ref)
					}
					if err != nil {
						commitErrors[ref] = err
						r.Failure("GitHub", p.Repository+" ref "+ref, err)
					} else {
						commits[ref] = commit
					}
				}
				v.Freshness = Resolve(snapshot, ref, commit, p.Repository, now())
				if err != nil {
					v.Freshness.Status = "Partial"
					v.Freshness.Reason += "; commit lookup failed"
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
}

func Resolve(s Snapshot, ref string, commit Commit, repository string, now time.Time) report.Freshness {
	now = now.UTC()
	f := report.Freshness{Status: report.Unknown, Update: report.Unknown, Reason: "No unambiguous semantic upstream tag association", Commit: commit.SHA, RetrievedAt: &now}
	futureDate := false
	matches := []string{}
	for _, tag := range s.Tags {
		if tag.Name == ref {
			matches = []string{tag.Name}
			break
		}
		if commit.SHA != "" && tag.SHA == commit.SHA {
			matches = append(matches, tag.Name)
		}
	}
	if len(matches) == 0 {
		for _, release := range s.Releases {
			if release.Tag == ref {
				matches = append(matches, ref)
				break
			}
		}
	}
	slices.Sort(matches)
	matches = slices.Compact(matches)
	if len(matches) == 1 {
		f.ResolvedTag = matches[0]
	} else if len(matches) > 1 {
		f.Reason = "Multiple tags/streams identify the source commit"
	}
	if f.ResolvedTag != "" {
		f.EvidenceURL = repository + "/releases/tag/" + url.PathEscape(f.ResolvedTag)
		for _, release := range s.Releases {
			if release.Tag == f.ResolvedTag && !release.Unstable && release.Date != nil {
				f.Date, f.AgeKind = release.Date, "release age"
			}
		}
	}
	if f.Date == nil && commit.Date != nil {
		f.Date, f.AgeKind = commit.Date, "commit age"
		f.EvidenceURL = repository + "/commit/" + url.PathEscape(commit.SHA)
	}
	if f.Date != nil {
		if !f.Date.After(now) {
			days := int(now.Sub(*f.Date).Hours() / 24)
			f.AgeDays = &days
		} else {
			futureDate = true
			f.Reason += "; provider date is in the future"
		}
	}
	prefix, built := splitTag(f.ResolvedTag)
	if built == nil {
		return f
	}
	unstable := map[string]bool{}
	all := map[string]bool{}
	for _, release := range s.Releases {
		if release.Unstable {
			unstable[release.Tag] = true
		}
		all[release.Tag] = true
	}
	for _, tag := range s.Tags {
		all[tag.Name] = true
	}
	var latest, line *semver.Version
	tags := make([]string, 0, len(all))
	for tag := range all {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	for _, tag := range tags {
		stream, version := splitTag(tag)
		if version == nil || stream != prefix || version.Prerelease() != "" || unstable[tag] {
			continue
		}
		if latest == nil || version.GreaterThan(latest) {
			latest, f.LatestStable = version, tag
		}
		if version.Major() == built.Major() && version.Minor() == built.Minor() && (line == nil || version.GreaterThan(line)) {
			line, f.LatestInLine = version, tag
		}
	}
	if latest == nil {
		f.Reason = "No stable tags in selected component stream"
		return f
	}
	f.Status, f.Reason, f.Update = "Available", "Compared within source-associated tag stream", "Current"
	if latest.GreaterThan(built) {
		switch {
		case latest.Major() > built.Major():
			f.Update = "Major"
		case latest.Minor() > built.Minor():
			f.Update = "Minor"
		default:
			f.Update = "Patch"
		}
	}
	f.LineUpdate = line != nil && line.GreaterThan(built)
	if built.Prerelease() != "" {
		f.Reason += "; selected source is a prerelease"
	}
	if futureDate {
		f.Reason += "; provider date is in the future; age unknown"
	}
	return f
}
