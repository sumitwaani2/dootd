package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasesAndDownload(t *testing.T) {
	// The "storage" host serves signed asset URLs; it must never see the token.
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token forwarded to the storage host")
		}
		w.Write([]byte("tarball bytes"))
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/repos/me/app/releases":
			w.Write([]byte(`[{"id":3,"tag_name":"v3","draft":true},{"id":2,"tag_name":"v2","name":"Two","assets":[{"id":20,"name":"app-linux-amd64.tar.gz","size":13}]},{"id":1,"tag_name":"v1"}]`))
		case "/repos/me/app/releases/tags/v2":
			w.Write([]byte(`{"id":2,"tag_name":"v2","assets":[{"id":20,"name":"app-linux-amd64.tar.gz","size":13}]}`))
		case "/repos/me/app/releases/assets/20":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("Accept %q", r.Header.Get("Accept"))
			}
			http.Redirect(w, r, storage.URL+"/signed/20", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	ctx := context.Background()
	a := &API{Token: "tok", Base: api.URL}
	repo := Repo{Owner: "me", Name: "app"}
	rs, err := a.Releases(ctx, repo, 10)
	if err != nil || len(rs) != 2 || rs[0].Tag != "v2" || rs[1].Tag != "v1" {
		t.Fatalf("drafts must be skipped, order kept: %+v %v", rs, err)
	}
	if rs, _ := a.Releases(ctx, repo, 1); len(rs) != 1 {
		t.Fatalf("limit: %+v", rs)
	}
	rel, err := a.ReleaseByTag(ctx, repo, "v2")
	if err != nil {
		t.Fatal(err)
	}
	asset, ok := rel.Asset("app-linux-amd64.tar.gz")
	if !ok {
		t.Fatal("asset")
	}
	if _, err := a.ReleaseByTag(ctx, repo, "v9"); err == nil || !strings.Contains(err.Error(), "has no release v9") {
		t.Fatalf("missing tag: %v", err)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "a.tar.gz")
	if err := a.Download(ctx, repo, asset.ID, p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "tarball bytes" {
		t.Fatalf("got %q", b)
	}
	if err := a.Download(ctx, repo, asset.ID, filepath.Join(dir, "small"), 4); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("size limit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "small")); err == nil {
		t.Fatal("a too-large download must not be left behind")
	}

	// Without the token the private repository looks missing.
	anon := &API{Base: api.URL}
	if _, err := anon.Releases(ctx, repo, 10); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "GitHub token") {
		t.Fatalf("private repo without token: %v", err)
	}
}
