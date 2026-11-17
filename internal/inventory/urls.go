package inventory

import (
	"net/url"
	"strings"
)

// RepositoryURL removes transport differences and credentials, never preserving
// query strings (which can contain access tokens).
func RepositoryURL(raw string) string {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "git+"))
	if !strings.Contains(raw, "://") {
		if i := strings.Index(raw, ":"); i > 0 && strings.Contains(raw[:i], "@") && !strings.Contains(raw[:i], "/") {
			raw = "ssh://" + raw[:i] + "/" + raw[i+1:]
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") {
		return ""
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if p == "" || strings.ContainsAny(p, "<>\"\\ \t\r\n") {
		return ""
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "." || segment == ".." {
			return ""
		}
	}
	host := strings.ToLower(u.Host)
	hostname := strings.ToLower(u.Hostname())
	if u.Scheme == "ssh" || u.Scheme == "git" || u.Port() == "443" && u.Scheme == "https" || u.Port() == "80" && u.Scheme == "http" {
		host = hostname
	}
	if hostname == "github.com" {
		p = strings.TrimSuffix(strings.ToLower(p), ".git")
		if len(strings.Split(p, "/")) != 2 {
			return ""
		}
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/" + p}).String()
}

func WebsiteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.Host = strings.ToLower(u.Host)
	return strings.TrimSuffix(u.String(), "/")
}
