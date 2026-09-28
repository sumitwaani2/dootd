// Package github fetches app source code (shallow git clones) and talks to
// the GitHub REST API for token validation and repo listing.
//
// The personal access token is only ever sent to github.com. Local
// file:// repositories are accepted for development and tests.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Repo is a normalized repository location.
type Repo struct {
	URL    string // clone URL
	Owner  string // github.com only
	Name   string // github.com only
	Local  bool   // file:// repository (dev/test)
	GitHub bool
}

var (
	shortRe  = regexp.MustCompile(`^([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// ParseRepo accepts "owner/repo", "github.com/owner/repo",
// "https://github.com/owner/repo(.git)" and "file:///abs/path".
func ParseRepo(s string) (Repo, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "file://") {
		p := strings.TrimPrefix(s, "file://")
		if !filepath.IsAbs(p) {
			return Repo{}, fmt.Errorf("file repo %q must use an absolute path (file:///...)", s)
		}
		return Repo{URL: s, Local: true}, nil
	}
	orig := s
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	m := shortRe.FindStringSubmatch(s)
	if m == nil {
		return Repo{}, fmt.Errorf("unsupported repo %q: use https://github.com/<owner>/<repo>", orig)
	}
	return Repo{
		URL:    "https://github.com/" + m[1] + "/" + m[2] + ".git",
		Owner:  m[1],
		Name:   m[2],
		GitHub: true,
	}, nil
}

// ValidateBranch checks a branch name for safety and basic git rules.
func ValidateBranch(b string) error {
	if !branchRe.MatchString(b) || strings.Contains(b, "..") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") || strings.HasPrefix(b, "-") {
		return fmt.Errorf("invalid branch name %q", b)
	}
	return nil
}

// Checkout is the result of a clone.
type Checkout struct {
	SHA     string
	Subject string // first line of the commit message
}

// Clone shallow-clones branch of repo into dir (which must not exist) and
// removes the .git directory. token is used only for github.com repos.
func Clone(ctx context.Context, repo Repo, branch, token, dir string) (Checkout, error) {
	if err := ValidateBranch(branch); err != nil {
		return Checkout{}, err
	}
	var auth transport.AuthMethod
	if repo.GitHub && token != "" {
		auth = &githttp.BasicAuth{Username: "x-access-token", Password: token}
	}
	r, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           repo.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	})
	if err != nil {
		return Checkout{}, cloneError(repo, branch, token, err)
	}
	head, err := r.Head()
	if err != nil {
		return Checkout{}, fmt.Errorf("read HEAD: %w", err)
	}
	co := Checkout{SHA: head.Hash().String()}
	if c, err := r.CommitObject(head.Hash()); err == nil {
		co.Subject, _, _ = strings.Cut(strings.TrimSpace(c.Message), "\n")
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return Checkout{}, err
	}
	return co, nil
}

func cloneError(repo Repo, branch, token string, err error) error {
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		if token == "" {
			return fmt.Errorf("clone %s: repository is private or missing; set a GitHub token with Contents: read access", redact(repo.URL))
		}
		return fmt.Errorf("clone %s: the GitHub token cannot read this repository (needs Contents: read)", redact(repo.URL))
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("clone %s: repository not found (or the token has no access to it)", redact(repo.URL))
	case errors.Is(err, plumbing.ErrReferenceNotFound), strings.Contains(err.Error(), "couldn't find remote ref"):
		return fmt.Errorf("clone %s: branch %q not found", redact(repo.URL), branch)
	}
	return fmt.Errorf("clone %s (branch %s): %w", redact(repo.URL), branch, err)
}

func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.User == nil {
		return u
	}
	p.User = nil
	return p.String()
}
