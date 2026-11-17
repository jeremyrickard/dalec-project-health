package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v84/github"
)

func TestPaginationExhaustionRequiresReadableLinks(t *testing.T) {
	for _, tc := range []struct {
		name, header  string
		current, next int
		fail          bool
	}{
		{"empty first page", "", 0, 0, false},
		{"last page with previous link", `<https://example.invalid?page=1>; rel="prev"`, 2, 0, false},
		{"advance", `<https://example.invalid?page=3>; rel="next"`, 2, 3, false},
		{"nonadvancing", "", 2, 2, true},
		{"malformed next", `<https://example.invalid?page=invalid>; rel="next"`, 0, 0, true},
		{"unquoted relation", `<https://example.invalid?page=invalid>; rel=next`, 0, 0, true},
		{"relation list", `<https://example.invalid?after=cursor>; rel="next last"`, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			header.Set("Link", tc.header)
			next, err := nextGitHubPage(&github.Response{Response: &http.Response{Header: header}, NextPage: tc.next}, tc.current)
			if (err != nil) != tc.fail || !tc.fail && next != tc.next {
				t.Fatalf("next=%d err=%v", next, err)
			}
		})
	}
}

func TestReleasePaginationFailurePreservesIncompleteSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			w.Header().Set("Link", `<https://api.github.com/repos/example/repo/releases?page=invalid>; rel="next"`)
			fmt.Fprint(w, `[{"id":1,"tag_name":"v1.0.0","published_at":"2026-10-01T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/tags"):
			t.Error("tag enrichment should not hide release pagination failure")
		default:
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		}
	}))
	defer server.Close()
	g := NewGitHub("", server.Client())
	g.Client.BaseURL, _ = url.Parse(server.URL + "/")
	s, err := g.Snapshot(context.Background(), "https://github.com/example/repo")
	if err == nil || s.ReleasesComplete || len(s.Releases) != 1 || s.DefaultBranch != "trunk" {
		t.Fatalf("release snapshot falsely claimed completeness: %+v %v", s, err)
	}
}
