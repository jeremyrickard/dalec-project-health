package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/netx"
	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

type providerClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *providerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *providerClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.now = c.now.Add(d)
		c.waits = append(c.waits, d)
	}
	return nil
}

func resilientProvider(server *httptest.Server) (*GitHub, *netx.GitHubTransport, *providerClock) {
	client := netx.NewGitHub(time.Second)
	tr := client.Transport.(*netx.GitHubTransport)
	clock := &providerClock{now: time.Now().UTC().Truncate(time.Second)}
	tr.Base, tr.Interval, tr.Now, tr.Sleep = server.Client().Transport, 0, clock.Now, clock.Sleep
	tr.Jitter = func(time.Duration) time.Duration { return 0 }
	// WithAuthToken copies the SDK client: disable its preflight checks after
	// that copy as well as sharing the resulting pointer with azcu.
	g := NewGitHub("synthetic-token", client)
	g.Now = clock.Now
	g.Client.BaseURL, _ = url.Parse(server.URL + "/")
	return g, tr, clock
}

func TestProviderCoordinatesSDKPreflightReleasesAndAzcuTags(t *testing.T) {
	var clock *providerClock
	calls := map[string]int{}
	arrivals := map[string][]time.Time{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.URL.Path + "?page=" + r.URL.Query().Get("page")
		calls[key]++
		arrivals[key] = append(arrivals[key], clock.Now())
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/releases?page=2>; rel=\"next\"", server.URL))
			} else if calls[key] == 1 {
				w.WriteHeader(502)
				return
			}
			fmt.Fprint(w, `[{"tag_name":"v1.0.0","published_at":"2026-10-01T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/tags"):
			if r.URL.Query().Get("page") == "" {
				if calls[key] == 1 {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(403)
					fmt.Fprint(w, `{"message":"secondary rate limit"}`)
					return
				}
				w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/tags?page=2>; rel=\"next\"", server.URL))
			}
			fmt.Fprint(w, `[{"name":"v1.0.0","commit":{"sha":"abc"}}]`)
		default:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(clock.Now().Add(2*time.Minute).Unix(), 10))
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		}
	}))
	defer server.Close()
	g, _, c := resilientProvider(server)
	clock = c
	start := clock.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s, err := g.Snapshot(ctx, "https://github.com/example/repo")
	if err != nil || len(s.Tags) != 2 || len(s.Releases) != 2 || !s.ReleasesComplete || !g.Client.DisableRateLimitCheck {
		t.Fatalf("shared SDK/azcu rate policy failed: %+v / %v", s, err)
	}
	if clock.Now().Sub(start) != 128*time.Second || calls["/repos/example/repo?page="] != 1 ||
		calls["/repos/example/repo/releases?page="] != 1 || calls["/repos/example/repo/releases?page=2"] != 2 ||
		calls["/repos/example/repo/tags?page="] != 2 || calls["/repos/example/repo/tags?page=2"] != 1 ||
		arrivals["/repos/example/repo/releases?page="][0].Sub(start) != 2*time.Minute {
		t.Fatalf("replayed completed pages or burst before reset: %v / waits %v", calls, clock.waits)
	}
}

func TestProviderSDKPreflightWaitCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10))
		fmt.Fprint(w, `{"default_branch":"trunk"}`)
	}))
	defer server.Close()
	g, tr, _ := resilientProvider(server)
	waiting := make(chan struct{})
	tr.Sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			close(waiting)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := g.Snapshot(ctx, "https://github.com/example/repo")
		done <- err
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("SDK failed before reaching the coordinated wait: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not reach rate wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 || !strings.Contains(err.Error(), "rate-limit wait interrupted") {
			t.Fatalf("cancellation lost or extra request sent: %v / %d", err, calls.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rate wait did not cancel promptly")
	}
}

func TestProviderActivityRetriesOnlyFailedPage(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(strconv.FormatBool(permanent), func(t *testing.T) {
			calls := map[string]int{}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				key := r.URL.Path + "?page=" + r.URL.Query().Get("page")
				calls[key]++
				if strings.HasSuffix(r.URL.Path, "/branches/trunk") {
					fmt.Fprint(w, `{"commit":{"sha":"pinned"}}`)
					return
				}
				if r.URL.Query().Get("sha") != "pinned" {
					t.Error("default-branch pinned head was lost on retry")
				}
				if r.URL.Query().Get("page") == "" {
					w.Header().Set("Link", fmt.Sprintf("<%s/repos/example/repo/commits?page=2>; rel=\"next\"", server.URL))
				} else if permanent || calls[key] == 1 {
					w.WriteHeader(503)
					return
				}
				fmt.Fprintf(w, `[{"sha":%q,"author":{"id":1,"type":"User"},"parents":[{"sha":"parent"}],"commit":{"committer":{"date":"2026-11-16T00:00:00Z"}}}]`, key)
			}))
			defer server.Close()
			g, _, _ := resilientProvider(server)
			a, err := g.Activity(context.Background(), "https://github.com/example/repo", completeActivitySnapshot(), activityAsOf)
			if calls["/repos/example/repo/branches/trunk?page="] != 1 || calls["/repos/example/repo/commits?page="] != 1 {
				t.Fatal("successful page progress was discarded")
			}
			if permanent {
				if err == nil || a.WindowComplete || a.Band != report.Unknown || a.DefaultBranchCommits.Count != nil ||
					a.DefaultBranchCommits.Observed != 1 || calls["/repos/example/repo/commits?page=2"] != 5 ||
					!strings.Contains(err.Error(), "page 2") || !strings.Contains(err.Error(), "HTTP 503") {
					t.Fatalf("exhausted later page looked complete or lost context: %+v / %v", a, err)
				}
			} else if err != nil || !a.WindowComplete || a.DefaultBranchCommits.Count == nil ||
				*a.DefaultBranchCommits.Count != 2 || calls["/repos/example/repo/commits?page=2"] != 2 {
				t.Fatalf("later page did not recover: %+v / %v", a, err)
			}
		})
	}
}

func TestProviderTerminalFailuresAreCredentialSafe(t *testing.T) {
	for _, status := range []int{401, 403, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("X-RateLimit-Remaining", "57")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"permission or invalid ref; ghp_secret-body https://user:password@host/ Authorization: Bearer secret"}`)
			}))
			defer server.Close()
			g, _, clock := resilientProvider(server)
			_, err := g.Commit(context.Background(), "https://github.com/example/repo", "https://user:password@host/ref?access_token=ghp_secret-ref")
			if err == nil || calls.Load() != 1 || len(clock.waits) != 0 || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(status)) {
				t.Fatalf("terminal error retried or lost status: %v", err)
			}
			for _, secret := range []string{"secret", "password", "Authorization", "access_token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("credential leaked in diagnostic: %s", err)
				}
			}
		})
	}
}

func TestProviderRetainsReleaseAgesWhenTagsExhaustRetries(t *testing.T) {
	var tags, commits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags"):
			tags.Add(1)
			w.WriteHeader(503)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			fmt.Fprint(w, `[{"id":1,"tag_name":"v1.0.0","published_at":"2026-10-01T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/branches/trunk"):
			fmt.Fprint(w, `{"commit":{"sha":"pinned"}}`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/commits/"):
			commits.Add(1)
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		}

	}))
	defer server.Close()
	g, _, _ := resilientProvider(server)
	r := report.New(activityAsOf)
	r.Projects = []report.Project{{Repository: "https://github.com/example/repo", Components: []report.Component{{
		Versions: []report.Version{{Ref: "v1.0.0"}},
	}}}}
	Enrich(context.Background(), r, g, func() time.Time { return activityAsOf })
	f := r.Projects[0].Components[0].Versions[0].Freshness
	a := r.Projects[0].Activity
	if !r.Partial || f.Status != "Partial" || f.Date == nil || f.AgeKind != "release age" || f.Update != report.Unknown ||
		f.LatestStable != "" || tags.Load() != netx.GitHubAttempts || commits.Load() != 0 || a == nil || !a.WindowComplete {
		t.Fatalf("lost safe evidence, fabricated freshness, or multiplied azcu retries: %+v / %+v", f, a)
	}
}

func TestProviderDistinguishesTimeoutScopes(t *testing.T) {
	for _, scope := range []string{"request", "upstream", "run"} {
		t.Run(scope, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			defer server.Close()
			g, tr, _ := resilientProvider(server)
			ctx := context.Background()
			want := "request timeout"
			switch scope {
			case "request":
				tr.RequestTimeout, tr.MaxAttempts = 20*time.Millisecond, 2
			case "upstream":
				var cancel context.CancelFunc
				ctx, cancel = netx.WithWorkingBudget(ctx, 20*time.Millisecond)
				defer cancel()
				want = "upstream working budget exhausted"
			case "run":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeoutCause(ctx, 20*time.Millisecond, netx.ErrRunDeadline)
				defer cancel()
				want = "generation/run deadline exhausted"
			}
			_, err := g.Commit(ctx, "https://github.com/example/repo", "v1.0.0")
			var e *netx.FetchError
			if !errors.As(err, &e) || !strings.Contains(e.Kind, want) ||
				!strings.Contains(err.Error(), `ref "v1.0.0"`) || e.Attempts == 0 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("scope/cause/request context lost: %v", err)
			}
		})
	}
}
