package netx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRequestTimeout = 60 * time.Second
	GitHubAttempts        = 5
)

// GitHubTransport is the sole retry/rate coordinator for one generation's
// GitHub client, including exported azcu helpers using that same client.
type GitHubTransport struct {
	Base           http.RoundTripper
	Interval       time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	Now            func() time.Time
	Sleep          func(context.Context, time.Duration) error
	Jitter         func(time.Duration) time.Duration
	gate           chan struct{}
	next           time.Time
	rateUntil      time.Time
	rateKind       string
	rateStatus     int
	cache          map[string]cached
}

func NewGitHub(requestTimeout time.Duration) *http.Client {
	return &http.Client{Transport: &GitHubTransport{
		Base: http.DefaultTransport, Interval: 110 * time.Millisecond,
		RequestTimeout: requestTimeout, MaxAttempts: GitHubAttempts,
		Now: time.Now, Sleep: Sleep, Jitter: func(limit time.Duration) time.Duration {
			return time.Duration(rand.Int64N(int64(max(limit, 1))))
		},
		gate: make(chan struct{}, 1), cache: make(map[string]cached),
	}}
}

func (t *GitHubTransport) failure(req *http.Request, kind string, status, attempts int, cause error) *FetchError {
	e := &FetchError{
		Operation: requestOperation(req), Kind: kind, Status: status,
		Attempts: attempts, MaxAttempts: t.MaxAttempts, Cause: cause,
	}
	if t.rateUntil.After(t.Now()) {
		e.RetryAt = t.rateUntil
	}
	return e
}

func (t *GitHubTransport) RoundTrip(req *http.Request) (_ *http.Response, returnedErr error) {
	defer func() { traceFailure(req.Context(), returnedErr) }()
	if req.Method != http.MethodGet {
		return nil, &FetchError{Operation: "request", Kind: "only read-only GET requests are supported"}
	}
	resume := pauseWork(req.Context())
	select {
	case <-req.Context().Done():
		resume()
		return nil, &FetchError{Operation: requestOperation(req), Kind: ContextError(req.Context()).Error(), Cause: ContextError(req.Context())}
	case t.gate <- struct{}{}:
		resume()
		defer func() { <-t.gate }()
	}
	if req.Context().Err() != nil {
		return nil, t.failure(req, ContextError(req.Context()).Error(), 0, 0, ContextError(req.Context()))
	}
	key := req.URL.String()
	if c, ok := t.cache[key]; ok {
		if c.status == http.StatusNotFound {
			return nil, t.failure(req, HTTPFailure(c.status)+"; cached result", c.status, 0, nil)
		}
		return response(req, c), nil
	}
	for attempt := 1; attempt <= t.MaxAttempts; attempt++ {
		if err := t.wait(req, attempt-1); err != nil {
			return nil, err
		}
		t.next = t.Now().Add(t.Interval)
		ctx, cancel := context.WithTimeout(req.Context(), t.RequestTimeout)
		res, err := t.Base.RoundTrip(req.Clone(ctx))
		var body []byte
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
			closeErr := res.Body.Close()
			if err == nil {
				err = closeErr
			}
		}
		attemptErr := ctx.Err()
		cancel()
		rate := false
		if res != nil {
			rate = t.observeRate(res, body, attempt)
		}
		if req.Context().Err() != nil {
			return nil, t.failure(req, ContextError(req.Context()).Error(), statusOf(res), attempt, ContextError(req.Context()))
		}
		// A terminal status stays terminal even if its error body is truncated.
		// Do not turn a known invalid ref or permission failure into a retry.
		if res != nil && res.StatusCode >= 400 && res.StatusCode < 500 && !rate {
			if res.StatusCode == http.StatusNotFound && err == nil && len(body) <= 32<<20 {
				t.cache[key] = cached{res.StatusCode, res.Header.Clone(), body}
			}
			return nil, t.failure(req, HTTPFailure(res.StatusCode), res.StatusCode, attempt, err)
		}
		if err != nil {
			if attemptErr != nil {
				err = attemptErr
			}
			if attempt == t.MaxAttempts {
				return nil, t.failure(req, FailureKind(req.Context(), err)+"; retries exhausted", statusOf(res), attempt, err)
			}
			if err := t.backoff(req, attempt, res, err); err != nil {
				return nil, err
			}
			continue
		}
		if len(body) > 32<<20 {
			return nil, t.failure(req, "response exceeds 32 MiB limit", res.StatusCode, attempt, nil)
		}
		if rate {
			if attempt == t.MaxAttempts {
				return nil, t.failure(req, t.rateKind+" rate limit; retries exhausted", res.StatusCode, attempt, nil)
			}
			continue
		}
		if res.StatusCode >= 500 && res.StatusCode <= 599 {
			if attempt == t.MaxAttempts {
				return nil, t.failure(req, "server failure; retries exhausted", res.StatusCode, attempt, nil)
			}
			if err := t.backoff(req, attempt, res, nil); err != nil {
				return nil, err
			}
			continue
		}
		c := cached{res.StatusCode, res.Header.Clone(), body}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			t.cache[key] = c
		}
		if res.StatusCode >= 400 {
			return nil, t.failure(req, HTTPFailure(res.StatusCode), res.StatusCode, attempt, nil)
		}
		return response(req, c), nil
	}
	return nil, t.failure(req, "retry attempts exhausted", 0, t.MaxAttempts, nil)
}

func statusOf(res *http.Response) int {
	if res != nil {
		return res.StatusCode
	}
	return 0
}

func (t *GitHubTransport) wait(req *http.Request, attempts int) error {
	ctx := req.Context()
	if ctx.Err() != nil {
		return t.failure(req, ContextError(ctx).Error(), t.rateStatus, attempts, ContextError(ctx))
	}
	for wait := t.rateUntil.Sub(t.Now()); wait > 0; wait = t.rateUntil.Sub(t.Now()) {
		if deadline, ok := ctx.Deadline(); ok && wait >= time.Until(deadline) {
			return t.failure(req, t.rateKind+" rate-limit wait exceeds remaining run budget", t.rateStatus, attempts, ErrRunDeadline)
		}
		resume := pauseWork(ctx)
		err := t.Sleep(ctx, wait)
		resume()
		if err != nil {
			return t.failure(req, t.rateKind+" rate-limit wait interrupted: "+ContextError(ctx).Error(), t.rateStatus, attempts, ContextError(ctx))
		}
	}
	if err := t.Sleep(ctx, max(t.next.Sub(t.Now()), 0)); err != nil {
		return t.failure(req, ContextError(ctx).Error(), 0, attempts, ContextError(ctx))
	}
	return nil
}

func (t *GitHubTransport) backoff(req *http.Request, attempt int, res *http.Response, lastErr error) error {
	delay := min(time.Second<<min(attempt-1, 5), 30*time.Second)
	delay += t.Jitter(delay / 4)
	if res != nil && !t.rateUntil.After(t.Now()) {
		if after, ok := retryAfter(res.Header.Get("Retry-After"), t.Now()); ok {
			delay = max(delay, after)
		}
	}
	retryAt := t.Now().Add(delay)
	if err := t.Sleep(req.Context(), delay); err != nil {
		lastKind := "server failure"
		if lastErr != nil {
			lastKind = FailureKind(context.WithoutCancel(req.Context()), lastErr)
		}
		e := t.failure(req, lastKind+"; retry interrupted: "+ContextError(req.Context()).Error(), statusOf(res), attempt, errors.Join(ContextError(req.Context()), lastErr))
		e.RetryAt = maxTime(e.RetryAt, retryAt)
		return e
	}
	return nil
}

func retryAfter(value string, now time.Time) (time.Duration, bool) {
	if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && n >= 0 && n <= int64((1<<63-1)/time.Second) {
		return time.Duration(n) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(date.Sub(now), 0), true
	}
	return 0, false
}

func (t *GitHubTransport) observeRate(res *http.Response, body []byte, attempt int) bool {
	now := t.Now()
	delay, retryHeader := retryAfter(res.Header.Get("Retry-After"), now)
	until := now.Add(delay)
	remaining, remainingErr := strconv.ParseInt(strings.TrimSpace(res.Header.Get("X-RateLimit-Remaining")), 10, 64)
	primary := remainingErr == nil && remaining == 0
	var message struct{ Message string }
	if res.StatusCode == http.StatusForbidden {
		_ = json.Unmarshal(body, &message)
	}
	text := strings.ToLower(message.Message)
	secondary := strings.Contains(text, "secondary rate limit") || strings.Contains(text, "abuse detection")
	rate := res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusForbidden &&
		(primary || retryHeader || secondary || strings.Contains(text, "rate limit exceeded"))
	if !rate && !primary {
		return false
	}
	known := retryHeader
	if primary {
		if n, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && n > 0 && n < 253402300800 {
			until = maxTime(until, time.Unix(n, 0))
			known = true
		}
	}
	if !known {
		until = now.Add(min(time.Minute<<min(attempt-1, 4), 15*time.Minute))
	}
	until = maxTime(until, now.Add(time.Second<<min(attempt-1, 5)))
	until = until.Add(t.Jitter(min(until.Sub(now)/4, 5*time.Second)))
	if until.After(t.rateUntil) {
		t.rateUntil, t.rateStatus = until, res.StatusCode
		t.rateKind = "provider"
		if secondary {
			t.rateKind = "secondary"
		}
		if primary || strings.Contains(text, "api rate limit exceeded") {
			t.rateKind = "primary"
		}
	}
	return rate
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
