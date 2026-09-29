package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const maxListedBranches = 100

type Branch struct {
	Name      string `json:"name"`
	SHA       string `json:"sha,omitempty"`
	Protected bool   `json:"protected,omitempty"`
	Default   bool   `json:"default,omitempty"`
}

func ValidGitHubName(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		ok := r == '.' || r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func SortBranches(defaultName string, branches []Branch) []Branch {
	defaultName = strings.TrimSpace(defaultName)
	out := append([]Branch(nil), branches...)
	for i := range out {
		out[i].Default = defaultName != "" && out[i].Name == defaultName
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if out == nil {
		out = []Branch{}
	}
	return out
}

func (c *Client) ListBranches(ctx context.Context, installationID int64, owner, repo, knownDefault string) (string, []Branch, error) {
	if !ValidGitHubName(owner) || !ValidGitHubName(repo) {
		return "", nil, fmt.Errorf("invalid repository")
	}
	token, err := c.branchToken(installationID)
	if err != nil {
		return "", nil, err
	}
	defaultBranch := strings.TrimSpace(knownDefault)
	if defaultBranch == "" {
		info, err := c.repoInfo(ctx, token, owner, repo)
		if err != nil {
			return "", nil, err
		}
		defaultBranch = info
	}
	path := fmt.Sprintf("%s/repos/%s/%s/branches?per_page=100", c.githubAPI(), url.PathEscape(owner), url.PathEscape(repo))
	var branches []Branch
	for path != "" && len(branches) < maxListedBranches {
		raw, header, err := c.tokenGET(ctx, token, path)
		if err != nil {
			return "", nil, err
		}
		var payload []struct {
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
			Commit    struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return "", nil, fmt.Errorf("github branches: invalid response")
		}
		for _, item := range payload {
			name := strings.TrimSpace(item.Name)
			if name == "" {
				continue
			}
			branches = append(branches, Branch{
				Name:      name,
				SHA:       item.Commit.SHA,
				Protected: item.Protected,
			})
			if len(branches) >= maxListedBranches {
				break
			}
		}
		path = nextLink(header.Get("Link"))
	}
	if branches == nil {
		branches = []Branch{}
	}
	return defaultBranch, SortBranches(defaultBranch, branches), nil
}

func (c *Client) githubAPI() string {
	if base := strings.TrimSpace(c.apiBase); base != "" {
		return strings.TrimRight(base, "/")
	}
	return "https://api.github.com"
}

func (c *Client) repoInfo(ctx context.Context, token, owner, repo string) (string, error) {
	raw, _, err := c.tokenGET(ctx, token, fmt.Sprintf("%s/repos/%s/%s", c.githubAPI(), url.PathEscape(owner), url.PathEscape(repo)))
	if err != nil {
		return "", err
	}
	var payload struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("github repo: invalid response")
	}
	return strings.TrimSpace(payload.DefaultBranch), nil
}

func (c *Client) branchToken(installationID int64) (string, error) {
	if installationID != 0 {
		return c.InstallationToken(installationID)
	}
	if c.Enabled() {
		return "", fmt.Errorf("github installation required")
	}
	return strings.TrimSpace(c.cfg.GitHubToken), nil
}

func (c *Client) tokenGET(ctx context.Context, token, rawURL string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("repository not found")
	}
	if resp.StatusCode == http.StatusConflict {
		return []byte("[]"), resp.Header, nil
	}
	if resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("github branches: %s", truncate(string(raw), 200))
	}
	return raw, resp.Header, nil
}
