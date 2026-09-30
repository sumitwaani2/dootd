package app

import (
	"testing"

	"github.com/sumitwaani2/dootd/internal/github"
)

func TestNameFromRepo(t *testing.T) {
	ok := map[string]string{
		"blog":        "blog",
		"My_Blog":     "my-blog",
		"site.v2":     "site-v2",
		"sample-zig":  "sample-zig",
		"a1":          "a1",
		"guestbook-c": "guestbook-c",
	}
	for in, want := range ok {
		if got, err := NameFromRepo(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"1blog", "x", "blog-", "-blog", "a-very-long-repository-name-x", "blog!", ""} {
		if got, err := NameFromRepo(in); err == nil {
			t.Errorf("%q: accepted as %q", in, got)
		}
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/me/My_Blog": "My_Blog",
		"github.com/me/site.git":        "site",
		"me/sample-zig":                 "sample-zig",
		"https://github.com/me/blog/":   "blog",
	} {
		r, err := github.ParseRepo(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if r.Name != want || r.Owner != "me" {
			t.Errorf("%s: got %+v, want %q", in, r, want)
		}
	}
	for _, in := range []string{"file:///srv/x.git", "https://gitlab.com/me/x", "me", "me/..", "-me/x"} {
		if r, err := github.ParseRepo(in); err == nil {
			t.Errorf("%s: accepted as %+v", in, r)
		}
	}
}
