package netx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRetriesCacheAndRateLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	client := New(0)
	for range 2 {
		res, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(body) != `{"ok":true}` {
			t.Fatal("response not replayable")
		}
	}
	if calls != 2 {
		t.Fatalf("want retry then cache hit, got %d calls", calls)
	}
}

func TestBoundedRetriesAndCancellation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "42")
		w.WriteHeader(429)
	}))
	defer server.Close()
	client := New(0)
	res, err := client.Get(server.URL)
	if err != nil || res.StatusCode != 429 || calls != 1 {
		t.Fatalf("long rate limit should not retry: %v, %d", err, calls)
	}

	res.Body.Close()
	_, err = client.Get(server.URL)
	if err == nil || !strings.Contains(err.Error(), "retry budget") || calls != 1 {
		t.Fatal("did not honor exhausted rate limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("ignored cancellation")
	}
	start := time.Now()
	if err := Sleep(ctx, time.Hour); err == nil || time.Since(start) > time.Second {
		t.Fatal("sleep not cancellable")
	}
}

func TestServerErrorRetryBudget(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
	}))
	defer server.Close()
	client := New(0)
	transport := client.Transport.(*Transport)
	transport.Sleep = func(context.Context, time.Duration) error { return nil }
	res, err := client.Get(server.URL)
	if err != nil || res.StatusCode != 503 || calls != 3 {
		t.Fatalf("retry budget wrong: %d calls %v", calls, err)
	}
	res.Body.Close()
}
