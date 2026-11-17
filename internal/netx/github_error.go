package netx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FetchError retains a machine-inspectable cause without printing request URLs,
// response bodies, headers, or the possibly credential-bearing original error.
type FetchError struct {
	Operation   string
	Kind        string
	Status      int
	Attempts    int
	MaxAttempts int
	RetryAt     time.Time
	Cause       error
}

type fetchTraceKey struct{}
type fetchTrace struct {
	mu      sync.Mutex
	failure *FetchError
}

// TraceFetches preserves a final transport failure when the SDK replaces it
// with ctx.Err() during cancellation. Traces are scoped to one provider call.
func TraceFetches(ctx context.Context) context.Context {
	return context.WithValue(ctx, fetchTraceKey{}, &fetchTrace{})
}

func TracedError(ctx context.Context, err error) error {
	if trace, ok := ctx.Value(fetchTraceKey{}).(*fetchTrace); ok {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		if trace.failure != nil {
			return trace.failure
		}
	}
	return err
}

func traceFailure(ctx context.Context, err error) {
	trace, ok := ctx.Value(fetchTraceKey{}).(*fetchTrace)
	var failure *FetchError
	if ok && errors.As(err, &failure) {
		trace.mu.Lock()
		trace.failure = failure
		trace.mu.Unlock()
	}
}

func (e *FetchError) Error() string {
	var parts []string
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.MaxAttempts > 0 {
		parts = append(parts, fmt.Sprintf("attempts %d/%d, retries %d", e.Attempts, e.MaxAttempts, max(e.Attempts-1, 0)))
	}
	if !e.RetryAt.IsZero() {
		parts = append(parts, "retry-at "+e.RetryAt.UTC().Format(time.RFC3339))
	}
	text := e.Kind
	if len(parts) != 0 {
		text += " (" + strings.Join(parts, "; ") + ")"
	}
	if e.Operation != "" {
		text = "GitHub " + e.Operation + ": " + text
	}
	return text
}

func (e *FetchError) Unwrap() error { return e.Cause }

func SafeReference(value string) string {
	lower := strings.ToLower(value)
	if strings.ContainsAny(value, ":?\r\n\\") || strings.Contains(lower, "github_pat_") {
		return "[redacted]"
	}
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.Contains(lower, prefix) {
			return "[redacted]"
		}
	}
	if len(value) > 160 {
		return "[long reference redacted]"
	}
	return strconv.QuoteToASCII(value)
}

func Operation(name, repository, ref string, page int) string {
	u, err := url.Parse(repository)
	repo := "[unknown repository]"
	if err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			repo = SafeReference(parts[0] + "/" + parts[1])
		}
	}
	label := name + " " + repo
	if ref != "" {
		label += " ref " + SafeReference(ref)
	}
	if page > 0 {
		label += fmt.Sprintf(" page %d", page)
	}
	return label
}

func requestOperation(req *http.Request) string {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" {
		return "GET request"
	}
	repository := "https://github.com/" + parts[1] + "/" + parts[2]
	if len(parts) == 3 {
		return Operation("metadata", repository, "", 0)
	}
	if len(parts) > 4 {
		return Operation(parts[3], repository, strings.Join(parts[4:], "/"), 0)
	}
	page, _ := strconv.Atoi(req.URL.Query().Get("page"))
	ref := ""
	if parts[3] == "commits" {
		ref = req.URL.Query().Get("sha")
	}
	return Operation(parts[3], repository, ref, max(page, 1))
}

func HTTPFailure(status int) string {
	switch status {
	case 400:
		return "invalid request (not retried)"
	case 401:
		return "authentication failed (not retried)"
	case 403:
		return "permission denied (not a rate limit; not retried)"
	case 404:
		return "not found or inaccessible (not retried)"
	case 422:
		return "invalid ref/request (not a rate limit; not retried)"
	default:
		return "HTTP request failed"
	}
}

func FailureKind(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return ContextError(ctx).Error()
	}
	var n net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timeout"
	case errors.As(err, &n) && n.Timeout():
		return "request timeout"
	case errors.Is(err, syscall.ECONNRESET):
		return "network failure: connection reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "network failure: connection refused"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "network failure: unexpected EOF"
	default:
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return "network failure: DNS lookup failed"
		}
		return "network failure"
	}
}
