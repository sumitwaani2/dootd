package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, cur string
		want     bool
	}{
		{"v1.0.0", "v0.9.9", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.1", false},
		{"v1.10.0", "v1.9.0", true},
		{"v1.0.0", "v1.0.0-rc1", true},
		{"v1.0.0-rc2", "v1.0.0-rc1", true},
		{"v1.0.0-rc1", "v1.0.0", false},
		{"v1.0.1", "v1.0.0-3-gabcdef0", true},
		{"v1.0.0", "v1.0.0-3-gabcdef0-dirty", false},
		{"v1.0.0", "dev", true},
		{"v1.0.0", "abc1234", true},
		{"latest", "v1.0.0", false},
		{"v1.0", "v0.1.0", false},
	} {
		if got := Newer(c.tag, c.cur); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.tag, c.cur, got, c.want)
		}
	}
	if !IsDevBuild("dev") || !IsDevBuild("v1.0.0-2-gabc1234") || IsDevBuild("v1.0.0") {
		t.Error("IsDevBuild")
	}
}

// fakeBinary is a shell script that answers `version` like dootd.
func fakeBinary(tag string) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\n[ \"$1\" = version ] && echo 'dootd %s (commit x, built y)'\nexit 0\n", tag))
}

type fakeGitHub struct {
	*httptest.Server
	bin, sums []byte
}

func newFakeGitHub(t *testing.T, tag string, bin []byte, badSum bool) *fakeGitHub {
	f := &fakeGitHub{bin: bin}
	sum := sha256.Sum256(bin)
	s := hex.EncodeToString(sum[:])
	if badSum {
		s = strings.Repeat("0", 64)
	}
	f.sums = []byte(s + "  " + AssetName() + "\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag, "name": tag, "html_url": "https://example/rel",
			"assets": []map[string]string{
				{"name": AssetName(), "browser_download_url": f.URL + "/dl/bin"},
				{"name": "checksums.txt", "browser_download_url": f.URL + "/dl/sums"},
			},
		})
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(f.bin) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { w.Write(f.sums) })
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func setup(t *testing.T) (bin, data string) {
	dir := t.TempDir()
	bin = filepath.Join(dir, "dootd")
	os.WriteFile(bin, fakeBinary("v1.0.0"), 0o755)
	data = filepath.Join(dir, "data")
	os.Mkdir(data, 0o711)
	return bin, data
}

func TestApplyGuardFinish(t *testing.T) {
	bin, data := setup(t)
	gh := newFakeGitHub(t, "v1.1.0", fakeBinary("v1.1.0"), false)
	u := &Updater{Repo: "o/r", API: gh.URL, Current: "v1.0.0", Binary: bin, DataRoot: data}
	ctx := context.Background()
	rel, err := u.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.1.0" || !Newer(rel.Tag, u.Current) {
		t.Fatalf("latest %+v", rel)
	}
	os.WriteFile(filepath.Join(data, "dootd.db.pre-update"), []byte("stale"), 0o600)
	if err := u.Apply(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v1.1.0") {
		t.Fatal("binary not replaced")
	}
	if b, _ := os.ReadFile(bin + PrevSuffix); !strings.Contains(string(b), "v1.0.0") {
		t.Fatal("previous binary not kept")
	}
	if _, err := os.Stat(filepath.Join(data, "dootd.db.pre-update")); err == nil {
		t.Fatal("stale pre-update copy kept")
	}
	m, ok, _ := ReadMarker(data)
	if !ok || m.Status != StatusPending || m.From != "v1.0.0" || m.To != "v1.1.0" {
		t.Fatalf("marker %+v", m)
	}
	// First start of the new binary.
	if msg, err := Guard(bin, data); err != nil || !strings.Contains(msg, "start 1 of 3") {
		t.Fatalf("guard: %q %v", msg, err)
	}
	// An old binary must not claim the update.
	if _, ok, _ := Finish(data, "v1.0.0"); ok {
		t.Fatal("finish by the wrong version")
	}
	r, ok, err := Finish(data, "v1.1.0")
	if err != nil || !ok || !r.OK {
		t.Fatalf("finish: %+v %v %v", r, ok, err)
	}
	if _, ok, _ := ReadMarker(data); ok {
		t.Fatal("marker left behind")
	}
	if msg, _ := Guard(bin, data); msg != "" {
		t.Fatalf("guard without an update: %q", msg)
	}
}

func TestGuardRollsBack(t *testing.T) {
	bin, data := setup(t)
	gh := newFakeGitHub(t, "v2.0.0", fakeBinary("v2.0.0"), false)
	u := &Updater{Repo: "o/r", API: gh.URL, Current: "v1.0.0", Binary: bin, DataRoot: data}
	rel, _ := u.Latest(context.Background())
	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(data, "dootd.db")
	// The new binary copied the database and migrated it, then crashed.
	os.WriteFile(db+PreUpdateSuffix, []byte("old schema"), 0o600)
	os.WriteFile(db, []byte("new schema"), 0o600)
	os.WriteFile(db+"-wal", []byte("wal"), 0o600)
	for i := 1; i <= MaxAttempts; i++ {
		if _, err := Guard(bin, data); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v2.0.0") {
			t.Fatalf("rolled back too early (attempt %d)", i)
		}
	}
	msg, err := Guard(bin, data)
	if err != nil || !strings.Contains(msg, "went back to v1.0.0") {
		t.Fatalf("rollback: %q %v", msg, err)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v1.0.0") {
		t.Fatal("binary not restored")
	}
	if b, _ := os.ReadFile(db); string(b) != "old schema" {
		t.Fatalf("database not restored: %q", b)
	}
	if _, err := os.Stat(db + "-wal"); err == nil {
		t.Fatal("stale WAL kept")
	}
	// Further starts (now the old binary) are left alone.
	if msg, _ := Guard(bin, data); msg != "" {
		t.Fatalf("guard after rollback: %q", msg)
	}
	r, ok, _ := Finish(data, "v1.0.0")
	if !ok || r.OK || !strings.Contains(r.Detail, "did not start") {
		t.Fatalf("finish after rollback: %+v", r)
	}
}

func TestApplyRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		bin    []byte
		badSum bool
		want   string
	}{
		"checksum": {fakeBinary("v1.1.0"), true, "checksum mismatch"},
		"version":  {fakeBinary("v9.9.9"), false, "expected v1.1.0"},
		"not exec": {[]byte("\x7fELF garbage"), false, "does not run"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, data := setup(t)
			gh := newFakeGitHub(t, "v1.1.0", tc.bin, tc.badSum)
			u := &Updater{Repo: "o/r", API: gh.URL, Current: "v1.0.0", Binary: bin, DataRoot: data}
			rel, _ := u.Latest(context.Background())
			err := u.Apply(context.Background(), rel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "v1.0.0") {
				t.Fatal("binary changed")
			}
			for _, p := range []string{bin + newSuffix, bin + PrevSuffix, MarkerPath(data)} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("%s left behind", p)
				}
			}
		})
	}
}
