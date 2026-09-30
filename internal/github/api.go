// Package github is a minimal GitHub REST client: token validation, repo
// listing, and the releases an app is deployed from (docs/architecture.md
// §11). The personal access token is only sent to the API host.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/testenv"
)

// DefaultBase is the GitHub REST API.
const DefaultBase = "https://api.github.com"

// API is a minimal GitHub REST client.
type API struct {
	Token string
	Base  string // default DefaultBase (DOOTD_TEST_GITHUB_API in the E2E tests)
	HTTP  *http.Client
}

func (a *API) base() string {
	switch {
	case a.Base != "":
		return a.Base
	case testenv.GitHubAPI() != "":
		return testenv.GitHubAPI()
	}
	return DefaultBase
}

// Repo is a github.com repository.
type Repo struct {
	Owner string
	Name  string
}

// URL is the repository's web address.
func (r Repo) URL() string { return "https://github.com/" + r.Owner + "/" + r.Name }

// String is owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

var repoRe = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})$`)

// ParseRepo accepts "owner/repo", "github.com/owner/repo" and
// "https://github.com/owner/repo(.git)".
func ParseRepo(s string) (Repo, error) {
	orig := strings.TrimSpace(s)
	s = strings.TrimPrefix(orig, "https://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	m := repoRe.FindStringSubmatch(s)
	if m == nil || m[2] == "." || m[2] == ".." {
		return Repo{}, fmt.Errorf("unsupported repository %q: use https://github.com/<owner>/<repo>", orig)
	}
	return Repo{Owner: m[1], Name: m[2]}, nil
}

// TokenInfo describes what a token can do.
type TokenInfo struct {
	Login  string
	Scopes string // classic tokens only (X-OAuth-Scopes)
}

// RepoInfo is one repository the token can read.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// ErrNotFound means the API answered 404 (or 403 for private data without
// access).
var ErrNotFound = errors.New("not found")

func (a *API) do(ctx context.Context, path string, out any) (http.Header, error) {
	base := a.base()
	c := a.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("github: token is invalid or expired")
	case http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("github: %s: %w (HTTP %d; for a private repository, set a GitHub token with Contents: Read-only in Settings)", path, ErrNotFound, resp.StatusCode)
	default:
		return nil, fmt.Errorf("github: %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, fmt.Errorf("github: %s: %w", path, err)
		}
	}
	return resp.Header, nil
}

// Validate checks the token and returns the account it belongs to.
func (a *API) Validate(ctx context.Context) (TokenInfo, error) {
	var u struct {
		Login string `json:"login"`
	}
	h, err := a.do(ctx, "/user", &u)
	if err != nil {
		return TokenInfo{}, err
	}
	return TokenInfo{Login: u.Login, Scopes: h.Get("X-OAuth-Scopes")}, nil
}

// Repos lists repositories the token can access (first 300).
func (a *API) Repos(ctx context.Context) ([]RepoInfo, error) {
	var all []RepoInfo
	for page := 1; page <= 3; page++ {
		var rs []RepoInfo
		if _, err := a.do(ctx, fmt.Sprintf("/user/repos?per_page=100&sort=pushed&page=%d", page), &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			break
		}
	}
	return all, nil
}

// Release is a published GitHub release.
type Release struct {
	ID          int64     `json:"id"`
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Asset is a file attached to a release.
type Asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Asset returns the asset called name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// Releases returns up to n published releases of r, newest first (drafts
// are skipped).
func (a *API) Releases(ctx context.Context, r Repo, n int) ([]Release, error) {
	var all []Release
	if _, err := a.do(ctx, fmt.Sprintf("%s/releases?per_page=%d", repoPath(r), min(max(n, 1)+5, 100)), &all); err != nil {
		return nil, err
	}
	var out []Release
	for _, rel := range all {
		if !rel.Draft && len(out) < n {
			out = append(out, rel)
		}
	}
	return out, nil
}

// ReleaseByTag returns the release with the given tag.
func (a *API) ReleaseByTag(ctx context.Context, r Repo, tag string) (Release, error) {
	var rel Release
	if _, err := a.do(ctx, repoPath(r)+"/releases/tags/"+url.PathEscape(tag), &rel); err != nil {
		if errors.Is(err, ErrNotFound) {
			return rel, fmt.Errorf("%s has no release %s (for a private repository, set a GitHub token with Contents: Read-only in Settings)", r, tag)
		}
		return rel, err
	}
	if rel.Draft {
		return rel, fmt.Errorf("release %s of %s is a draft", tag, r)
	}
	return rel, nil
}

// Download writes release asset id of r to path (at most max bytes). The
// asset API redirects to a short-lived signed URL on another host; the
// token is not forwarded there.
func (a *API) Download(ctx context.Context, r Repo, id int64, path string, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%s/releases/assets/%d", a.base(), repoPath(r), id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	c := http.Client{} // no overall timeout: ctx bounds the download
	if a.HTTP != nil {
		c = *a.HTTP
	}
	// Never send the token anywhere but the API host (not even to another
	// port of it); Go's own rule compares host names only.
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if r.URL.Host != via[0].URL.Host {
			r.Header.Del("Authorization")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("github: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: download asset %d: HTTP %d", id, resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fmt.Errorf("asset is larger than %d MB", max>>20)
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("github: download: %w", err)
	}
	return nil
}
