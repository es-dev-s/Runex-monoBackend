package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"deploy_platform/internal/config"
)

// HTTPError is a GitHub API failure with a status code the control plane can classify.
type HTTPError struct {
	Op     string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "github: unknown error"
	}
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("github %s: %d", e.Op, e.Status)
	}
	return fmt.Sprintf("github %s: %d %s", e.Op, e.Status, body)
}

func httpErr(op string, status int, raw []byte) error {
	return &HTTPError{Op: op, Status: status, Body: truncate(string(raw), 200)}
}

func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == 404
}

func IsUnauthorized(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == 401 || he.Status == 403)
}

func IsCredentials(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "private key") || strings.Contains(msg, "invalid github app private key") || strings.Contains(msg, "parse github app private key") {
		return true
	}
	return strings.Contains(msg, "json web token") ||
		strings.Contains(msg, "could not be decoded") ||
		strings.Contains(msg, "invalid jwt")
}

type Client struct {
	cfg     config.Config
	http    *http.Client
	apiBase string
	mu      sync.Mutex
	tokens  map[int64]cachedToken
	// skew is GitHub's clock minus this machine. A fast server clock makes
	// App JWTs fail with 401 even when the private key is correct.
	skew     time.Duration
	skewTill time.Time
}

type cachedToken struct {
	token   string
	expires time.Time
}

type Installation struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	Type    string `json:"type"`
}

type Repo struct {
	FullName      string `json:"fullName"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"defaultBranch"`
	URL           string `json:"url"`
}

func New(cfg config.Config) *Client {
	return &Client{
		cfg:    cfg,
		http:   &http.Client{Timeout: 30 * time.Second},
		tokens: map[int64]cachedToken{},
	}
}

func (c *Client) Enabled() bool {
	return c.cfg.GitHubAppID != "" && c.cfg.GitHubAppPrivateKey != ""
}

func (c *Client) Slug() string {
	return c.cfg.GitHubAppSlug
}

func (c *Client) InstallURL(state string) string {
	slug := c.cfg.GitHubAppSlug
	if slug == "" {
		return "https://github.com/settings/apps"
	}
	u, _ := url.Parse("https://github.com/apps/" + url.PathEscape(slug) + "/installations/new")
	if state != "" {
		query := u.Query()
		query.Set("state", state)
		u.RawQuery = query.Encode()
	}
	return u.String()
}

func (c *Client) InvalidateToken(installationID int64) {
	c.mu.Lock()
	delete(c.tokens, installationID)
	c.mu.Unlock()
}

func (c *Client) InstallationToken(installationID int64) (string, error) {
	c.mu.Lock()
	if cached, ok := c.tokens[installationID]; ok && time.Until(cached.expires) > 2*time.Minute {
		token := cached.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	var last error
	for attempt := 0; attempt < 3; attempt++ {
		token, err := c.mintInstallationToken(installationID)
		if err == nil {
			return token, nil
		}
		last = err
		if !IsNotFound(err) {
			return "", err
		}
		time.Sleep(time.Duration(250*(attempt+1)) * time.Millisecond)
	}
	return "", last
}

func (c *Client) mintInstallationToken(installationID int64) (string, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", httpErr("token", resp.StatusCode, raw)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("github token: invalid response")
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

func (c *Client) GetInstallation(id int64) (Installation, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return Installation{}, err
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.github.com/app/installations/%d", id), nil)
	if err != nil {
		return Installation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return Installation{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return Installation{}, httpErr("installation", resp.StatusCode, raw)
	}
	var payload struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Installation{}, err
	}
	return Installation{ID: payload.ID, Account: payload.Account.Login, Type: payload.Account.Type}, nil
}

func (c *Client) GetApp() (string, error) {
	id, name, _, err := c.AppIdentity()
	if err != nil {
		return "", err
	}
	if id == 0 || name == "" {
		return "", fmt.Errorf("github app: invalid response")
	}
	return name, nil
}

func (c *Client) AppIdentity() (int64, string, string, error) {
	raw, err := c.appGET("https://api.github.com/app")
	if err != nil {
		return 0, "", "", err
	}
	var payload struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Name == "" {
		return 0, "", "", fmt.Errorf("github app: invalid response")
	}
	return payload.ID, payload.Name, payload.Slug, nil
}

func (c *Client) ListInstallations() ([]Installation, error) {
	raw, err := c.appGET("https://api.github.com/app/installations?per_page=100")
	if err != nil {
		return nil, err
	}
	var payload []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(payload))
	for _, item := range payload {
		out = append(out, Installation{ID: item.ID, Account: item.Account.Login, Type: item.Account.Type})
	}
	return out, nil
}

func (c *Client) appGET(url string) ([]byte, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return nil, httpErr("app", resp.StatusCode, raw)
	}
	return raw, nil
}

func (c *Client) ListRepos(installationID int64) ([]Repo, error) {
	repos, err := c.listReposOnce(installationID)
	if err != nil && IsUnauthorized(err) {
		c.InvalidateToken(installationID)
		return c.listReposOnce(installationID)
	}
	return repos, err
}

func (c *Client) listReposOnce(installationID int64) ([]Repo, error) {
	token, err := c.InstallationToken(installationID)
	if err != nil {
		return nil, err
	}
	var repos []Repo
	url := "https://api.github.com/installation/repositories?per_page=100"
	for url != "" {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, httpErr("repos", resp.StatusCode, raw)
		}
		var payload struct {
			Repositories []struct {
				Name          string `json:"name"`
				FullName      string `json:"full_name"`
				Private       bool   `json:"private"`
				DefaultBranch string `json:"default_branch"`
				HTMLURL       string `json:"html_url"`
				Owner         struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload.Repositories {
			repos = append(repos, Repo{
				FullName:      item.FullName,
				Name:          item.Name,
				Owner:         item.Owner.Login,
				Private:       item.Private,
				DefaultBranch: item.DefaultBranch,
				URL:           item.HTMLURL,
			})
		}
		url = nextLink(resp.Header.Get("Link"))
	}
	if repos == nil {
		repos = []Repo{}
	}
	return repos, nil
}

func (c *Client) githubNow() time.Time {
	c.mu.Lock()
	if !c.skewTill.IsZero() && time.Now().Before(c.skewTill) {
		skew := c.skew
		c.mu.Unlock()
		return time.Now().Add(skew)
	}
	c.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com", nil)
	if err != nil {
		return time.Now()
	}
	req.Header.Set("User-Agent", "runex")
	resp, err := c.http.Do(req)
	if err != nil {
		return time.Now()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	remote, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return time.Now()
	}
	skew := remote.Sub(time.Now())
	c.mu.Lock()
	c.skew = skew
	c.skewTill = time.Now().Add(10 * time.Minute)
	c.mu.Unlock()
	return time.Now().Add(skew)
}

func (c *Client) appJWT() (string, error) {
	key, err := parseRSAKey(c.cfg.GitHubAppPrivateKey)
	if err != nil {
		return "", err
	}
	// iat a bit in the past, exp <= 10m from iat (GitHub hard limit). githubNow()
	// corrects VPS clock skew that otherwise yields "exp too far in the future".
	now := c.githubNow().Add(-30 * time.Second)
	var iss any = c.cfg.GitHubAppID
	if n, err := strconv.ParseInt(c.cfg.GitHubAppID, 10, 64); err == nil {
		iss = n
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"iat": now.Unix(),
		"exp": now.Add(8 * time.Minute).Unix(),
		"iss": iss,
	})
	h := base64.RawURLEncoding.EncodeToString(header)
	p := base64.RawURLEncoding.EncodeToString(payload)
	unsigned := h + "." + p
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAKey(raw string) (*rsa.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("missing GitHub App private key")
	}
	if !strings.Contains(raw, "BEGIN") {
		path := raw
		if path == "~" || strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				if path == "~" {
					path = home
				} else {
					path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
				}
			}
		}
		if decoded, err := os.ReadFile(path); err == nil {
			raw = string(decoded)
		} else if data, err := base64.StdEncoding.DecodeString(raw); err == nil && strings.Contains(string(data), "BEGIN") {
			raw = string(data)
		}
	}
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, fmt.Errorf("invalid GitHub App private key PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("GitHub App private key must be RSA")
	}
	return key, nil
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start >= 0 && end > start {
			return part[start+1 : end]
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ParseID(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}
