// Package netx provides bounded, cancellable, read-only HTTP with request reuse.
package netx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type cached struct {
	status int
	header http.Header
	body   []byte
}

type Transport struct {
	Base     http.RoundTripper
	Interval time.Duration
	MaxWait  time.Duration
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
	mu       sync.Mutex
	next     time.Time
	cache    map[string]cached
}

func New(interval time.Duration) *http.Client {
	return &http.Client{Timeout: 25 * time.Second, Transport: &Transport{
		Base: http.DefaultTransport, Interval: interval, MaxWait: 5 * time.Second, Now: time.Now, Sleep: Sleep,
	}}
}

func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("only read-only GET requests are supported")
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	key := req.URL.String()
	if c, ok := t.cache[key]; ok {
		return response(req, c), nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		wait := t.next.Sub(t.Now())
		if wait > t.MaxWait {
			return nil, fmt.Errorf("provider rate-limit wait exceeds %s retry budget", t.MaxWait)
		}
		if err := t.Sleep(req.Context(), wait); err != nil {
			return nil, err
		}
		t.next = t.Now().Add(t.Interval)
		res, err := t.Base.RoundTrip(req)
		if err != nil {
			if attempt == 2 || req.Context().Err() != nil {
				return nil, fmt.Errorf("provider request failed (transport or cancellation)")
			}
			t.next = t.Now().Add(time.Duration(attempt+1) * time.Second)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 32<<20+1))
		closeErr := res.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("provider response could not be read completely")
		}
		if len(body) > 32<<20 {
			return nil, fmt.Errorf("provider response exceeds 32 MiB limit")
		}
		retry := res.StatusCode == 429 || res.StatusCode >= 500 || (res.StatusCode == 403 && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != ""))
		delay := time.Duration(attempt+1) * time.Second
		if s := res.Header.Get("Retry-After"); s != "" {
			if seconds, err := strconv.Atoi(s); err == nil {
				delay = time.Duration(seconds) * time.Second
			} else if date, err := http.ParseTime(s); err == nil {
				delay = date.Sub(t.Now())
			}
		} else if s := res.Header.Get("X-RateLimit-Reset"); s != "" && res.Header.Get("X-RateLimit-Remaining") == "0" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				if req.URL.Hostname() == "api.github.com" || n > 1000000000 {
					delay = time.Unix(n, 0).Sub(t.Now())
				} else {
					delay = time.Duration(n) * time.Second
				}
			}
		}
		if retry || res.Header.Get("X-RateLimit-Remaining") == "0" {
			t.next = t.Now().Add(max(delay, t.Interval))
		}
		c := cached{res.StatusCode, res.Header.Clone(), body}
		if retry && attempt < 2 && delay <= t.MaxWait {
			t.next = t.Now().Add(max(delay, t.Interval))
			continue
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 || res.StatusCode == 404 {
			if t.cache == nil {
				t.cache = make(map[string]cached)
			}
			t.cache[key] = c
		}
		return response(req, c), nil
	}
	return nil, fmt.Errorf("provider retry budget exhausted")
}

func response(req *http.Request, c cached) *http.Response {
	return &http.Response{StatusCode: c.status, Header: c.header.Clone(), Body: io.NopCloser(bytes.NewReader(c.body)), Request: req}
}
