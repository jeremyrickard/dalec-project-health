// Package inventory adapts exported azcu APIs for read-only spec inventory.
package inventory

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Azure/dalec-build-defs/azcu/pkg/spec"
	"github.com/distribution/reference"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

func Load(ctx context.Context, checkout string, now time.Time) (*report.Report, error) {
	root, err := filepath.Abs(checkout)
	if err != nil {
		return nil, err
	}
	specs := filepath.Join(root, "specs")
	info, err := os.Lstat(specs)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("input must contain a real specs/ directory")
	}
	// Preflight filesystem failures because the library walker assumes entries
	// are readable. Excluded fixture directories need not be readable.
	if err := filepath.WalkDir(specs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "test" || d.Name() == "tests" || d.Name() == "templates") {
			return fs.SkipDir
		}
		return ctx.Err()
	}); err != nil {
		return nil, err
	}
	r := report.New(now)
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	if out, err := cmd.Output(); err == nil {
		r.Source.Revision = strings.TrimSpace(string(out))
		out, err = exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", root, "status", "--porcelain", "--", "specs").Output()
		if err != nil {
			return nil, fmt.Errorf("read input checkout status: %w", err)
		}
		r.Source.Dirty = len(out) > 0
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	} else {
		r.Diagnostics = append(r.Diagnostics, report.Diagnostic{Provider: "inventory", Subject: "source revision", Message: "Unknown: input is not a readable Git checkout"})
	}
	projects := map[string]*report.Project{}
	count := 0
	err = spec.WalkSpecs(func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("production spec must be a regular file, not a symlink: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rawFile, err := os.Open(path)
		if err != nil {
			return err
		}
		parsed, parseErr := spec.Parse(rawFile)
		closeErr := rawFile.Close()
		if parseErr != nil {
			return parseErr
		}
		if closeErr != nil {
			return closeErr
		}
		var metadata map[string]any
		if err := parsed.ReadPath("$", &metadata); err != nil {
			return fmt.Errorf("%s: read YAML document: %w", relative, err)
		}
		identityFields := 0
		for _, key := range []string{"name", "version", "revision", "license"} {
			if _, ok := metadata[key]; ok {
				identityFields++
			}
		}
		_, hasExtensions := metadata["x-build-extensions"]
		_, hasRevision := metadata["revision"]
		if identityFields < 2 && !hasExtensions && !hasRevision {
			r.Diagnostics = append(r.Diagnostics, report.Diagnostic{Provider: "inventory", Subject: filepath.ToSlash(relative), Message: "Excluded ancillary YAML: no DALEC package identity signature"})
			return nil
		}
		s, err := spec.FromFile(path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if s.Name == "" || s.Version == "" || s.Revision == "" {
			return fmt.Errorf("%s: name, version and revision are required", path)
		}
		v := report.Version{
			PackageVersion: s.Version, Revision: s.Revision, Path: filepath.ToSlash(relative),
			Website: WebsiteURL(s.Website), Candidates: []report.Candidate{}, Images: []report.Image{},
			Freshness: report.Freshness{Status: report.Unknown, Update: report.Unknown, Reason: "Upstream not identified"},
		}
		var rawSources map[string]struct {
			Git *struct {
				Commit string `yaml:"commit"`
			} `yaml:"git"`
		}
		if len(s.Sources) > 0 {
			if err := parsed.ReadPath("$.sources", &rawSources); err != nil {
				return fmt.Errorf("%s: read raw source evidence: %w", relative, err)
			}
		}
		commitRepos := map[string]bool{}
		invalidGitIdentity := false
		for name, source := range s.Sources {
			if source.Git == nil {
				continue
			}
			repo := RepositoryURL(source.Git.URL)
			if repo == "" {
				invalidGitIdentity = true
				r.Diagnostics = append(r.Diagnostics, report.Diagnostic{Provider: "inventory", Subject: v.Path, Message: "Git source has unsupported or invalid repository URL"})
				continue
			}
			ref := source.Git.Commit
			if strings.ContainsAny(ref, " \t\r\n~^:?*[\\") {
				ref = ""
			}
			v.Candidates = append(v.Candidates, report.Candidate{Source: name, Repository: repo, Ref: ref})
			if raw := rawSources[name]; raw.Git != nil && (strings.Contains(raw.Git.Commit, "${COMMIT}") || raw.Git.Commit == "$COMMIT") {
				commitRepos[repo] = true
			}
		}
		slices.SortFunc(v.Candidates, func(a, b report.Candidate) int { return strings.Compare(a.Source, b.Source) })
		repo, reason := primary(v.Candidates, v.Website, commitRepos)
		if invalidGitIdentity {
			repo, reason = "", "A Git source identity is invalid or unsupported; primary repository is unknown"
		}
		refs := map[string]bool{}
		for _, c := range v.Candidates {
			if c.Repository == repo {
				refs[c.Ref] = true
			}
		}
		if len(refs) == 1 {
			for ref := range refs {
				v.Ref = ref
			}
			v.Freshness.Reason = "Enrichment not attempted"
		} else if repo != "" {
			v.Freshness.Reason = "Multiple source refs for the selected repository"
		}
		ext, err := spec.LoadExtensions(s)
		if err != nil {
			return fmt.Errorf("%s: build extensions: %w", relative, err)
		}
		v.Targets = slices.Clone(ext.BuildTargets)
		if len(v.Targets) == 0 {
			v.Targets = spec.DefaultBuildTargets()
		}
		sort.Strings(v.Targets)
		images := map[string][]string{}
		for _, target := range v.Targets {
			// Container outputs use the "/container" target, never rpm/deb.
			if target != "container" && !strings.HasSuffix(target, "/container") {
				continue
			}
			image, err := spec.ImageName(s, path, target)
			if err != nil {
				return fmt.Errorf("%s: image identity: %w", relative, err)
			}
			identity := "mcr.microsoft.com/" + strings.Trim(image.Repository, "/") + "/" + image.Name + ":" + image.Tag
			if _, err := reference.ParseNormalizedNamed(identity); err != nil {
				return fmt.Errorf("%s: invalid image identity", relative)
			}
			images[identity] = append(images[identity], target)
		}
		for id, targets := range images {
			v.Images = append(v.Images, report.Image{Identity: id, Targets: slices.Compact(targets)})
		}
		slices.SortFunc(v.Images, func(a, b report.Image) int { return strings.Compare(a.Identity, b.Identity) })
		key := repo
		if key == "" {
			key = "unknown:" + v.Path
		}
		p := projects[key]
		if p == nil {
			p = &report.Project{ID: key, Repository: repo, Reason: reason, Lifecycle: report.Unknown,
				Components: []report.Component{}, LFX: report.Health{Status: report.Unknown, Lifecycle: report.Unknown, Reason: "No verified LFX match"}}
			projects[key] = p
		}
		i := slices.IndexFunc(p.Components, func(c report.Component) bool { return c.Name == s.Name })
		if i < 0 {
			p.Components = append(p.Components, report.Component{Name: s.Name, Versions: []report.Version{}})
			i = len(p.Components) - 1
		}
		p.Components[i].Versions = append(p.Components[i].Versions, v)
		count++
		return nil
	}, spec.WithRootDir(specs), spec.WithFastFail())
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("no production specs found in input specs/")
	}
	for _, p := range projects {
		slices.SortFunc(p.Components, func(a, b report.Component) int { return strings.Compare(a.Name, b.Name) })
		r.Projects = append(r.Projects, *p)
	}
	slices.SortFunc(r.Projects, func(a, b report.Project) int { return strings.Compare(a.ID, b.ID) })
	return r, nil
}

func primary(candidates []report.Candidate, website string, commitRepos map[string]bool) (string, string) {
	repos := map[string]bool{}
	for _, c := range candidates {
		repos[c.Repository] = true
	}
	if len(repos) == 0 {
		return "", "No usable Git source"
	}
	if len(repos) == 1 {
		for repo := range repos {
			return repo, "Sole Git repository"
		}
	}
	evidence := map[string]bool{}
	if repo := RepositoryURL(website); repos[repo] {
		evidence[repo] = true
	}
	if len(commitRepos) == 1 {
		for repo := range commitRepos {
			evidence[repo] = true
		}
	}
	if len(evidence) == 1 {
		for repo := range evidence {
			return repo, "Unique website or raw COMMIT reference evidence"
		}
	}
	return "", "Multiple Git repositories; primary evidence is absent or conflicting"
}
