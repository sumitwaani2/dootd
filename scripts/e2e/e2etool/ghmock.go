package main

// ghmock: a fake GitHub REST API serving releases from a directory.
//
//	e2etool ghmock -listen ADDR -dir DIR [-token T]
//
// Layout: DIR/<owner>/<repo>/<NNN>-<tag>/<asset files>. Higher NNN is newer.
// Markers: DIR/<owner>/<repo>/private (the token is required), a file
// "draft" in a release directory (a draft release), a file "name" (the
// release name), and <asset>.delay
// holding seconds to wait before serving that asset (slow downloads).

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ghAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	path string
}

type ghRelease struct {
	ID          int64     `json:"id"`
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

func ghmock(args []string) {
	fs := flag.NewFlagSet("ghmock", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8902", "listen address")
	dir := fs.String("dir", ".", "releases directory")
	token := fs.String("token", "", "token required for private repos and /user")
	fs.Parse(args)

	authed := func(r *http.Request) bool {
		return *token != "" && r.Header.Get("Authorization") == "Bearer "+*token
	}
	json200 := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	notFound := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}
	releases := func(owner, repo string) ([]ghRelease, bool) {
		base := filepath.Join(*dir, owner, repo)
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, false
		}
		var out []ghRelease
		for _, e := range entries {
			n, tag, ok := strings.Cut(e.Name(), "-")
			num, err := strconv.Atoi(n)
			if !e.IsDir() || !ok || err != nil {
				continue
			}
			rd := filepath.Join(base, e.Name())
			st, _ := os.Stat(rd)
			rel := ghRelease{ID: int64(num), Tag: tag, Name: "Release " + tag, PublishedAt: st.ModTime().UTC(),
				Prerelease: strings.Contains(tag, "-rc")}
			if _, err := os.Stat(filepath.Join(rd, "draft")); err == nil {
				rel.Draft = true
			}
			if b, err := os.ReadFile(filepath.Join(rd, "name")); err == nil {
				rel.Name = strings.TrimSpace(string(b))
			}
			files, _ := os.ReadDir(rd)
			for _, f := range files {
				if f.IsDir() || f.Name() == "draft" || f.Name() == "name" || strings.HasSuffix(f.Name(), ".delay") {
					continue
				}
				fi, _ := f.Info()
				p := filepath.Join(rd, f.Name())
				rel.Assets = append(rel.Assets, ghAsset{ID: int64(crc32.ChecksumIEEE([]byte(p)) & 0x7fffffff), Name: f.Name(), Size: fi.Size(), path: p})
			}
			out = append(out, rel)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
		return out, true
	}
	visible := func(r *http.Request, owner, repo string) bool {
		if _, err := os.Stat(filepath.Join(*dir, owner, repo)); err != nil {
			return false
		}
		if _, err := os.Stat(filepath.Join(*dir, owner, repo, "private")); err == nil {
			return authed(r)
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json200(w, map[string]any{"login": "e2e"})
	})
	mux.HandleFunc("GET /user/repos", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		owners, _ := os.ReadDir(*dir)
		for _, o := range owners {
			repos, _ := os.ReadDir(filepath.Join(*dir, o.Name()))
			for _, rp := range repos {
				if rp.IsDir() && visible(r, o.Name(), rp.Name()) {
					out = append(out, map[string]any{"full_name": o.Name() + "/" + rp.Name(), "private": true, "default_branch": "main"})
				}
			}
		}
		json200(w, out)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases", func(w http.ResponseWriter, r *http.Request) {
		o, rp := r.PathValue("owner"), r.PathValue("repo")
		rs, ok := releases(o, rp)
		if !ok || !visible(r, o, rp) {
			notFound(w)
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if n <= 0 || n > 100 {
			n = 30
		}
		if len(rs) > n {
			rs = rs[:n]
		}
		if rs == nil {
			rs = []ghRelease{}
		}
		json200(w, rs)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		o, rp := r.PathValue("owner"), r.PathValue("repo")
		rs, ok := releases(o, rp)
		if !ok || !visible(r, o, rp) {
			notFound(w)
			return
		}
		for _, rel := range rs {
			// Like GitHub, drafts are not found by tag.
			if rel.Tag == r.PathValue("tag") && !rel.Draft {
				json200(w, rel)
				return
			}
		}
		notFound(w)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		o, rp := r.PathValue("owner"), r.PathValue("repo")
		rs, ok := releases(o, rp)
		if !ok || !visible(r, o, rp) {
			notFound(w)
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		for _, rel := range rs {
			for _, a := range rel.Assets {
				if a.ID != id {
					continue
				}
				if r.Header.Get("Accept") != "application/octet-stream" {
					json200(w, a)
					return
				}
				// Like GitHub: a redirect to a signed URL.
				http.Redirect(w, r, fmt.Sprintf("/_storage/%d?path=%s", id, a.path), http.StatusFound)
				return
			}
		}
		notFound(w)
	})
	mux.HandleFunc("GET /_storage/{id}", func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Clean(r.URL.Query().Get("path"))
		if !strings.HasPrefix(p, filepath.Clean(*dir)+"/") {
			notFound(w)
			return
		}
		if b, err := os.ReadFile(p + ".delay"); err == nil {
			if s, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				time.Sleep(time.Duration(s) * time.Second)
			}
		}
		http.ServeFile(w, r, p)
	})
	log.Printf("ghmock listening on %s, serving %s", *listen, *dir)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
