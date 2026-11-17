package upstream

import (
	"fmt"
	"strings"

	"github.com/google/go-github/v84/github"
)

func nextGitHubPage(response *github.Response, current int) (int, error) {
	if response.NextPage != 0 {
		if response.NextPage <= max(current, 1) {
			return 0, fmt.Errorf("GitHub pagination did not advance")
		}
		return response.NextPage, nil
	}
	// A next relation the client could not parse is not proof of exhaustion.
	for _, header := range response.Header.Values("Link") {
		for _, link := range strings.Split(header, ",") {
			for _, attribute := range strings.Split(link, ";")[1:] {
				key, value, ok := strings.Cut(attribute, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
					continue
				}
				for _, relation := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
					if strings.EqualFold(relation, "next") {
						return 0, fmt.Errorf("GitHub pagination exposes an unreadable next page")
					}
				}
			}
		}
	}
	return 0, nil
}
