package inventory

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

func TestReadOnlyCheckoutSmoke(t *testing.T) {
	root := os.Getenv("DALEC_HEALTH_CHECKOUT")
	if root == "" {
		t.Skip("set DALEC_HEALTH_CHECKOUT to opt in to a local checkout smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := Load(ctx, root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	count, components, upstreams := 0, 0, 0
	for _, p := range r.Projects {
		if p.Repository != "" {
			upstreams++
		}
		components += len(p.Components)
		for _, c := range p.Components {
			count += len(c.Versions)
		}
	}
	if expected := os.Getenv("DALEC_HEALTH_EXPECTED_SPEC_COUNT"); expected != "" {
		want, err := strconv.Atoi(expected)
		if err != nil || want != count {
			t.Fatalf("inventory count %d does not match expected %q", count, expected)
		}
	}
	t.Logf("project groups=%d upstreams=%d components=%d spec variants=%d diagnostics=%d", len(r.Projects), upstreams, components, count, len(r.Diagnostics))
}

func TestRepositoryURL(t *testing.T) {
	for _, raw := range []string{
		"https://user:secret@github.com/Example/Widget.git?token=secret#frag",
		"git@github.com:Example/Widget.git", "git+https://github.com/Example/Widget",
		"ssh://git@github.com/Example/Widget.git",
		"ssh://git@GitHub.com:22/Example/Widget.GIT",
		"https://GitHub.com:443/Example/Widget.git",
	} {
		if got := RepositoryURL(raw); got != "https://github.com/example/widget" {
			t.Errorf("%s => %q", raw, got)
		}
	}
	for _, raw := range []string{"file:///secret", "javascript:alert(1)", "https://github.com/org", "garbage"} {
		if got := RepositoryURL(raw); got != "" {
			t.Errorf("accepted %q: %q", raw, got)
		}
	}
}

func TestPrimaryEvidence(t *testing.T) {
	a, b := "https://github.com/example/a", "https://github.com/example/b"
	tests := []struct {
		name       string
		candidates []report.Candidate
		website    string
		commit     map[string]bool
		want       string
	}{
		{"none", nil, "", nil, ""},
		{"sole", []report.Candidate{{Repository: a}, {Repository: a}}, "", nil, a},
		{"ambiguous", []report.Candidate{{Repository: a}, {Repository: b}}, "", nil, ""},
		{"website", []report.Candidate{{Repository: a}, {Repository: b}}, a, nil, a},
		{"commit", []report.Candidate{{Repository: a}, {Repository: b}}, "", map[string]bool{b: true}, b},
		{"conflict", []report.Candidate{{Repository: a}, {Repository: b}}, a, map[string]bool{b: true}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := primary(test.candidates, test.website, test.commit)
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func writeSpec(t *testing.T, root, name, extra string) {
	t.Helper()
	p := filepath.Join(root, "specs", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	base := "name: widget\nversion: 1.2.3\nrevision: \"1\"\ndescription: Synthetic widget\nwebsite: https://example.org\nlicense: MIT\n"
	if err := os.WriteFile(p, []byte(base+extra), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadVariantsTargetsAndExclusions(t *testing.T) {
	root := t.TempDir()
	sources := `
args:
  COMMIT: v1.2.3
sources:
  main:
    git:
      url: https://user:secret@github.com/example/widget.git?token=secret
      commit: ${COMMIT}
`
	writeSpec(t, root, "example/a.yml", sources+`
x-build-extensions:
  repository: example
  build-targets: [azlinux3/container, ubuntu/container, debian/container, azlinux3/rpm]
  per-target:
    ubuntu:
      repository: alternate
      image-name: special
`)
	writeSpec(t, root, "example/b.yaml", sources+"\nx-build-extensions:\n  repository: example\n")
	writeSpec(t, root, "example/c.yml", "\nx-build-extensions:\n  build-targets: [azlinux3/rpm]\n")
	writeSpec(t, root, "tests/ignore.yml", "invalid: yaml\n")
	writeSpec(t, root, "templates/ignore.yml", "invalid: yaml\n")
	writeSpec(t, root, "example/matrix.yml", "invalid: yaml\n")
	ancillary := filepath.Join(root, "specs", "example", "presubmit.yml")
	if err := os.WriteFile(ancillary, []byte("tasks:\n  linux: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/does/not/exist", filepath.Join(root, "specs", "tests", "ignored-link.yml")); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), root, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Projects) != 2 {
		t.Fatalf("want two projects, got %+v", r.Projects)
	}
	var grouped *report.Project
	for i := range r.Projects {
		p := &r.Projects[i]
		if p.Repository != "" {
			grouped = p
		} else if len(p.Components[0].Versions[0].Images) != 0 {
			t.Fatal("package-only spec has an image")
		}
	}
	if grouped == nil || len(grouped.Components) != 1 || len(grouped.Components[0].Versions) != 2 {
		t.Fatal("variants not retained")
	}
	v := grouped.Components[0].Versions[0]
	if v.Ref != "v1.2.3" || len(v.Images) != 2 {
		t.Fatalf("unexpected resolved inventory %+v", v)
	}
	for _, image := range v.Images {
		if strings.Contains(image.Identity, "alternate/special") && len(image.Targets) != 1 {
			t.Fatal("override association lost")
		}
		if strings.Contains(image.Identity, "example/widget") && len(image.Targets) != 2 {
			t.Fatal("default identity not deduplicated")
		}
	}
	if strings.Contains(grouped.Repository, "secret") {
		t.Fatal("credentials retained")
	}
	defaults := grouped.Components[0].Versions[1]
	if len(defaults.Targets) != 2 || len(defaults.Images) != 1 {
		t.Fatalf("defaults not applied: %+v", defaults)
	}
}

func TestLoadRawCommitAndAmbiguousRefs(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "example/a.yml", `
args:
  COMMIT: abcdef1234
sources:
  helper:
    git: {url: "https://github.com/else/helper", commit: v9.0.0}
  primary:
    git: {url: "https://github.com/example/widget", commit: "${COMMIT}"}
x-build-extensions: {repository: example}
`)
	r, err := Load(context.Background(), root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.Projects[0].Repository != "https://github.com/example/widget" || r.Projects[0].Components[0].Versions[0].Ref != "abcdef1234" {
		t.Fatalf("raw COMMIT evidence not respected: %+v", r.Projects)
	}
}

func TestConflictingRefsAndInvalidIdentityRemainUnknown(t *testing.T) {
	for _, invalidURL := range []bool{false, true} {
		root := t.TempDir()
		secondURL := "https://github.com/example/widget"
		if invalidURL {
			secondURL = "file:///private/source"
		}
		writeSpec(t, root, "example/a.yml", `
sources:
  a:
    git: {url: "https://github.com/example/widget", commit: v1.0.0}
  b:
    git: {url: "`+secondURL+`", commit: v2.0.0}
x-build-extensions: {repository: example}
`)
		r, err := Load(context.Background(), root, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		p := r.Projects[0]
		if invalidURL && p.Repository != "" {
			t.Fatal("invalid source was ignored when selecting primary")
		}
		if p.Components[0].Versions[0].Ref != "" {
			t.Fatal("conflicting refs were arbitrarily selected")
		}
	}
}

func TestInputFailures(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "malformed", "symlink", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind != "missing" {
				if err := os.Mkdir(filepath.Join(root, "specs"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "malformed":
				writeSpec(t, root, "bad.yml", "sources: [bad")
			case "symlink":
				if err := os.Symlink("/does/not/exist", filepath.Join(root, "specs", "bad.yml")); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			if _, err := Load(ctx, root, time.Now()); err == nil {
				t.Fatal("expected input error")
			}
		})
	}
}
