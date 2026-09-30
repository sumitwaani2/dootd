package web

import (
	"bytes"
	"testing"
	"time"

	"github.com/sumitwaani2/dootd/internal/apps"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/github"
	"github.com/sumitwaani2/dootd/internal/hostinfo"
)

// Templates are only checked when executed, so render the main pages with
// representative data: a field that no longer exists fails here, not in
// front of the user.
func TestTemplatesRender(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	row := AppRow{App: apps.App{Name: "web", Repo: "https://github.com/me/web", Domain: "web.example.com", Port: 20001}}
	rel := github.Release{Tag: "v2", Name: "Two", PublishedAt: time.Now()}
	pages := map[string]any{
		"home": map[string]any{"Apps": []AppRow{row}, "Host": hostinfo.Read(t.TempDir()), "Warnings": []string{"w"}},
		"app": map[string]any{"App": &row, "Range": "1h", "Ranges": []string{"1h"},
			"Releases":       []deployer.Release{{App: "web", ID: "v1", Subject: "One", SHA256: "0123456789abcdef", Current: true}},
			"GitHubReleases": []github.Release{rel, {Tag: "v1", Prerelease: true}},
			"Edit":           apps.Input{Repo: row.Repo}, "EdgeHost": edge.HostStatus{Host: "web.example.com"}},
		"app_new":  map[string]any{"In": apps.Input{RestoreFrom: apps.AutoFolder}, "BucketSet": true, "Folders": []string{"web"}},
		"settings": map[string]any{"Edge": edge.Status{Dashboard: "d.example.com", TokenSet: true}, "Policy": "every 3h"},
	}
	for name, data := range pages {
		var b bytes.Buffer
		if err := s.pages[name].ExecuteTemplate(&b, "layout", Page{Title: name, CSRF: "x", Data: data}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No releases yet: the page explains how to publish one.
	var b bytes.Buffer
	if err := s.pages["app"].ExecuteTemplate(&b, "layout", Page{Data: map[string]any{"App": &row}}); err != nil || !bytes.Contains(b.Bytes(), []byte("git tag v1.0.0")) {
		t.Errorf("app without releases: %v", err)
	}
}
