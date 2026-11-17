package netx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type testClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
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

func testGitHub() (*http.Client, *GitHubTransport, *testClock) {
	client := NewGitHub(time.Second)
	tr := client.Transport.(*GitHubTransport)
	clock := &testClock{now: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	tr.Interval, tr.Now, tr.Sleep = 0, clock.Now, clock.Sleep
	tr.Jitter = func(time.Duration) time.Duration { return 0 }
	return client, tr, clock
}

func get(client *http.Client, ctx context.Context, address string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	return err
}

func TestGitHubRateAndTransientRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		header     http.Header
		wait       time.Duration
	}{
		{"seconds", "", 429, http.Header{"Retry-After": {"12"}}, 12 * time.Second},
		{"http-date", "", 429, http.Header{"Retry-After": {"Tue, 17 Nov 2026 00:00:20 GMT"}}, 20 * time.Second},
		{"primary", "", 403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1794873720"}}, 2 * time.Minute},
		{"later-header-wins", "", 403, http.Header{"Retry-After": {"10"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1794873720"}}, 2 * time.Minute},
		{"secondary", `{"message":"You have exceeded a secondary rate limit."}`, 403, nil, time.Minute},
		{"abuse", `{"message":"You have triggered an abuse detection mechanism."}`, 403, nil, time.Minute},
		{"missing-headers", "", 429, nil, time.Minute},
		{"malformed-headers", "", 429, http.Header{"Retry-After": {"bad"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"bad"}}, time.Minute},
		{"overflow-reset", "", 429, http.Header{"Retry-After": {"9223372036854775807"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"9223372036854775807"}}, time.Minute},
		{"past-retry", "", 429, http.Header{"Retry-After": {"Mon, 16 Nov 2026 00:00:00 GMT"}}, time.Second},
		{"server", "", 502, nil, time.Second},
		{"server-retry-after", "", 503, http.Header{"Retry-After": {"15"}}, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, clock := testGitHub()
			start := clock.Now()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					for key, values := range tc.header {
						w.Header()[key] = values
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			if err := get(client, context.Background(), server.URL+"/repos/example/repo/releases?page=2"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || clock.Now().Sub(start) != tc.wait {
				t.Fatalf("calls=%d wait=%s, want 2/%s", calls.Load(), clock.Now().Sub(start), tc.wait)
			}
		})
	}
}

func TestGitHubTerminalStatusesAndCache(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			client, _, clock := testGitHub()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "malformed")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration: secret-body"}`)
			}))
			defer server.Close()
			start := clock.Now()
			for range 2 {
				err := get(client, context.Background(), server.URL+"/repos/example/repo/commits/v1.0.0?token=secret-query")
				var e *FetchError
				if !errors.As(err, &e) || e.Status != status || strings.Contains(e.Error(), "secret") {
					t.Fatalf("unsafe or lost failure: %+v", e)
				}
			}
			want := int32(2)
			if status == 404 {
				want = 1
			}
			if calls.Load() != want || clock.Now() != start {
				t.Fatalf("terminal response retried or waited: calls=%d", calls.Load())
			}
		})
	}
}

func TestGitHubRetryExhaustionAndJitter(t *testing.T) {
	client, tr, clock := testGitHub()
	tr.Jitter = func(limit time.Duration) time.Duration { return limit }
	start := clock.Now()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	err := get(client, context.Background(), server.URL+"/repos/example/repo/tags?page=3")
	var e *FetchError
	if !errors.As(err, &e) || e.Attempts != 5 || e.Status != 503 || !strings.Contains(e.Error(), "page 3") ||
		!strings.Contains(e.Error(), "retries 4") || calls.Load() != 5 || clock.Now().Sub(start) != 18750*time.Millisecond {
		t.Fatalf("unexpected exhausted error: %v / %d calls / %s", e, calls.Load(), clock.Now().Sub(start))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubNetworkAndReadFailuresRetry(t *testing.T) {
	for _, failure := range []error{syscall.ECONNRESET, io.ErrUnexpectedEOF} {
		t.Run(failure.Error(), func(t *testing.T) {
			client, tr, _ := testGitHub()
			calls := 0
			tr.Base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if errors.Is(failure, syscall.ECONNRESET) {
						return nil, failure
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(errorReader{failure})}, nil
				}
				return response(r, cached{status: 200, header: http.Header{}, body: []byte("{}")}), nil
			})
			if err := get(client, context.Background(), "https://api.github.com/repos/example/repo"); err != nil || calls != 2 {
				t.Fatalf("failure not retried: %v, %d", err, calls)
			}
		})
	}
	client, tr, _ := testGitHub()
	tr.Base = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, syscall.ECONNRESET })
	err := get(client, context.Background(), "https://api.github.com/repos/example/repo")
	var e *FetchError
	if !errors.As(err, &e) || !errors.Is(err, syscall.ECONNRESET) || !strings.Contains(e.Kind, "connection reset") {
		t.Fatalf("original network cause lost: %v", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestGitHubRequestTimeoutRetriesSameRequest(t *testing.T) {
	client, tr, _ := testGitHub()
	tr.RequestTimeout = 100 * time.Millisecond
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	if err := get(client, context.Background(), server.URL+"/repos/example/repo"); err != nil || calls.Load() != 2 {
		t.Fatalf("request timeout did not recover: %v, calls=%d", err, calls.Load())
	}
}

func TestGitHubSharedGateCacheAndCancellation(t *testing.T) {
	client, tr, clock := testGitHub()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(clock.Now().Add(20*time.Second).Unix(), 10))
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	address := server.URL + "/repos/example/repo"
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := get(client, context.Background(), address); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("cache misses were not coordinated: %d", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	tr.Sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			close(waiting)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- get(client, ctx, address+"/releases") }()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("request bypassed shared rate wait: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach rate wait")
	}
	// A second caller must be cancellable even while the coordinator is occupied.
	queued, stop := context.WithCancel(context.Background())
	defer stop()
	observed := &queueContext{Context: queued, entered: make(chan struct{})}
	queueDone := make(chan error, 1)
	go func() { queueDone <- get(client, observed, address+"/tags") }()
	select {
	case <-observed.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second request did not reach coordinator queue")
	}
	stop()
	var err error
	select {
	case err = <-queueDone:
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not cancel promptly")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queue ignored cancellation: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("rate wait ignored cancellation or sent another request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rate wait did not cancel promptly")
	}
}

func TestGitHubRateWaitTooLargeForRunBudget(t *testing.T) {
	client, _, clock := testGitHub()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := get(client, ctx, server.URL+"/repos/example/repo")
	var e *FetchError
	if !errors.As(err, &e) || !errors.Is(err, ErrRunDeadline) || e.Status != 429 ||
		!strings.Contains(e.Kind, "remaining run budget") || e.RetryAt.IsZero() || len(clock.waits) != 0 || calls.Load() != 1 {
		t.Fatalf("rate limit exceeded budget or lost cause: %v", err)
	}
}

func TestGitHubTruncatedTerminalResponseDoesNotRetry(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		client, tr, _ := testGitHub()
		calls := 0
		tr.Base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(errorReader{io.ErrUnexpectedEOF})}, nil
		})
		err := get(client, context.Background(), "https://api.github.com/repos/example/repo/commits/v1.0.0")
		var e *FetchError
		if !errors.As(err, &e) || e.Status != status || calls != 1 || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("terminal HTTP %d became a transient network failure: %v / %d", status, err, calls)
		}
	}
}

func TestGitHubRateRetriesAreBounded(t *testing.T) {
	client, tr, clock := testGitHub()
	calls := 0
	tr.Base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(r, cached{status: 429, header: http.Header{}}), nil
	})
	start := clock.Now()
	err := get(client, context.Background(), "https://api.github.com/repos/example/repo")
	var e *FetchError
	if !errors.As(err, &e) || e.Status != 429 || calls != 5 || e.Attempts != 5 ||
		e.RetryAt.IsZero() || clock.Now().Sub(start) != 15*time.Minute || !strings.Contains(e.Kind, "rate limit; retries exhausted") {
		t.Fatalf("unbounded or unqualified rate retries: %v / %d / %s", err, calls, clock.Now().Sub(start))
	}
}

type queueContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *queueContext) Value(key any) any {
	if _, ok := key.(budgetKey); ok {
		c.once.Do(func() { close(c.entered) })
	}
	return c.Context.Value(key)
}

func TestGitHubCancellationDuringRetryPreservesBothCauses(t *testing.T) {
	client, tr, _ := testGitHub()
	tr.Base = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, syscall.ECONNRESET
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	err := get(client, ctx, "https://api.github.com/repos/example/repo/commits/v1.0.0")
	var e *FetchError
	if !errors.As(err, &e) || !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.ECONNRESET) ||
		e.Attempts != 1 || e.RetryAt.IsZero() || !strings.Contains(e.Kind, "connection reset; retry interrupted") {
		t.Fatalf("retry cancellation lost original cause: %v", err)
	}
}
