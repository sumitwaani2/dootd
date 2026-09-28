package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// API is a minimal GitHub REST client.
type API struct {
	Token string
	Base  string // default https://api.github.com
	HTTP  *http.Client
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

func (a *API) do(ctx context.Context, path string, out any) (http.Header, error) {
	base := a.Base
	if base == "" {
		base = "https://api.github.com"
	}
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
		return nil, fmt.Errorf("github: %s: access denied or not found (HTTP %d)", path, resp.StatusCode)
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

// Branches lists up to 100 branch names of owner/name.
func (a *API) Branches(ctx context.Context, owner, name string) ([]string, error) {
	var bs []struct {
		Name string `json:"name"`
	}
	if _, err := a.do(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/branches?per_page=100", &bs); err != nil {
		return nil, err
	}
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out, nil
}
