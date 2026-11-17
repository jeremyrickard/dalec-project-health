package inventory

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestWorkerRootNestedAndDebugTargetsWithoutGitEvidence(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "example/worker.yml", `
args:
  COMMIT: orphan-argument-is-not-a-source-ref
x-build-extensions:
  repository: synthetic/workers/default
  image-name: worker
  build-targets: [azlinux3/worker, jammy/worker, windowscross, noble/testing/container, debug/worker, debug/sources, azlinux3/rpm, jammy/deb]
  per-target:
    jammy:
      repository: synthetic/workers/jammy
      image-name: jammy-worker
    windowscross:
      repository: synthetic/workers/windows
      image-name: cross-worker
`)
	r, err := Load(context.Background(), root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := r.Projects[0]
	v := p.Components[0].Versions[0]
	if p.Repository != "" || v.Ref != "" || len(v.Candidates) != 0 || p.Reason != "No usable Git source" {
		t.Fatal("orphan COMMIT argument became invented Git provenance")
	}
	want := map[string][]string{
		"mcr.microsoft.com/oss/v2/synthetic/workers/default/worker:1.2.3-1":       {"azlinux3/worker", "noble/testing/container"},
		"mcr.microsoft.com/oss/v2/synthetic/workers/jammy/jammy-worker:1.2.3-1":   {"jammy/worker"},
		"mcr.microsoft.com/oss/v2/synthetic/workers/windows/cross-worker:1.2.3-1": {"windowscross"},
	}
	if len(v.Images) != len(want) || len(v.Targets) != 8 {
		t.Fatalf("worker/root images dropped or debug/package targets emitted: %+v", v)
	}
	for _, image := range v.Images {
		if !slices.Equal(image.Targets, want[image.Identity]) {
			t.Fatalf("wrong worker publishing identity or associations: %+v", image)
		}
	}
}
