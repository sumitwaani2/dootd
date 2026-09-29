package backup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/sumitwaani2/dootd/internal/s3"
)

func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o700)
	os.WriteFile(filepath.Join(src, "app.db"), []byte("one"), 0o600)
	os.WriteFile(filepath.Join(src, "sub", "b.db"), []byte("two"), 0o600)
	m := &Manifest{Version: ManifestVersion, App: "web", Files: []ManifestFile{{Path: "app.db"}, {Path: "sub/b.db"}}}
	out := filepath.Join(t.TempDir(), "a.tar.zst")
	sum, size, err := writeArchive(out, m, src)
	if err != nil {
		t.Fatal(err)
	}
	if got, n, _ := fileSHA256(out); got != sum || n != size {
		t.Fatal("reported checksum/size do not match the file")
	}
	dst := t.TempDir()
	got, err := extractArchive(out, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got.App != "web" || len(got.Files) != 2 {
		t.Fatalf("%+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "b.db")); string(b) != "two" {
		t.Fatalf("content %q", b)
	}
}

// rawArchive builds a tar.zst with a manifest and entries (name -> body);
// the manifest lists paths with the right hashes unless lie is set.
func rawArchive(t testing.TB, paths []string, entries map[string]string, lie bool) string {
	var files []ManifestFile
	for _, p := range paths {
		body := entries["data/"+p]
		h := sha256.Sum256([]byte(body))
		sum := hex.EncodeToString(h[:])
		if lie {
			sum = strings.Repeat("0", 64)
		}
		files = append(files, ManifestFile{Path: p, Size: int64(len(body)), SHA256: sum})
	}
	mj, _ := json.Marshal(Manifest{Version: ManifestVersion, App: "web", CreatedAt: time.Now(), Files: files})
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Size: int64(len(mj)), Mode: 0o600, Typeflag: tar.TypeReg})
	tw.Write(mj)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0o600, Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	p := filepath.Join(t.TempDir(), "x.tar.zst")
	os.WriteFile(p, buf.Bytes(), 0o600)
	return p
}

func TestExtractRejects(t *testing.T) {
	for name, a := range map[string]struct {
		paths   []string
		entries map[string]string
		lie     bool
	}{
		"dotdot path":    {[]string{"../evil"}, map[string]string{"data/../evil": "x"}, false},
		"absolute path":  {[]string{"/evil"}, map[string]string{"data//evil": "x"}, false},
		"unlisted entry": {nil, map[string]string{"data/extra": "x"}, false},
		"missing file":   {[]string{"a.db"}, map[string]string{}, false},
		"wrong checksum": {[]string{"a.db"}, map[string]string{"data/a.db": "x"}, true},
		"unclean path":   {[]string{"a/./b"}, map[string]string{"data/a/./b": "x"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "out")
			os.Mkdir(dst, 0o700)
			if _, err := extractArchive(rawArchive(t, a.paths, a.entries, a.lie), dst); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); err == nil {
				t.Fatal("wrote outside the destination")
			}
		})
	}
}

// FuzzExtractArchive feeds arbitrary bytes (and valid archives as seeds):
// no panic, and nothing outside the destination.
func FuzzExtractArchive(f *testing.F) {
	seed := func(paths []string, entries map[string]string) {
		b, _ := os.ReadFile(rawArchive(f, paths, entries, false))
		f.Add(b)
	}
	seed([]string{"app.db"}, map[string]string{"data/app.db": "SQLite format 3\x00"})
	seed([]string{"a/b.db"}, map[string]string{"data/a/b.db": "x"})
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, b []byte) {
		root := t.TempDir()
		in := filepath.Join(root, "in.tar.zst")
		os.WriteFile(in, b, 0o600)
		dst := filepath.Join(root, "out")
		os.Mkdir(dst, 0o700)
		extractArchive(in, dst)
		es, _ := os.ReadDir(root)
		if len(es) != 2 {
			t.Fatalf("wrote outside the destination: %v", es)
		}
	})
}

func TestNewest(t *testing.T) {
	objs := []s3.Object{
		{Key: "web/20260101T000000Z-scheduled-1.tar.zst"},
		{Key: "web/20260103T000000Z-manual-9.tar.zst"},
		{Key: "web/20260102T000000Z-pre-deploy-5.tar.zst"},
		{Key: "web/notes.txt", LastModified: time.Now()},
	}
	o, ok := Newest(objs)
	if !ok || !strings.Contains(o.Key, "20260103") {
		t.Fatalf("%v %v", o, ok)
	}
	if _, ok := Newest(nil); ok {
		t.Fatal("empty list")
	}
	if Folder("web") != "web/" {
		t.Fatal("Folder")
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-48 * time.Hour)
	objs := []s3.Object{
		{Key: "web/20260929T090000Z-scheduled-4.tar.zst"}, // 3 h old
		{Key: "web/20260926T090000Z-scheduled-1.tar.zst"}, // 3 days old
		{Key: "web/20260927T120000Z-manual-2.tar.zst"},    // exactly 48 h: kept
		{Key: "web/20260925T090000Z-scheduled-0.tar.zst"}, // 4 days old
	}
	got := Expired(objs, cutoff)
	if len(got) != 2 || !strings.Contains(got[0].Key, "-1.") || !strings.Contains(got[1].Key, "-0.") {
		t.Fatalf("got %v", got)
	}
	// The newest backup is kept however old it is.
	old := []s3.Object{{Key: "web/20250101T000000Z-scheduled-1.tar.zst"}, {Key: "web/20240101T000000Z-scheduled-0.tar.zst"}}
	if got := Expired(old, cutoff); len(got) != 1 || !strings.Contains(got[0].Key, "2024") {
		t.Fatalf("newest must be kept: %v", got)
	}
}

func TestNextSlot(t *testing.T) {
	at := time.Date(2026, 9, 28, 4, 10, 0, 0, time.UTC)
	if got := nextSlot(at, 3*time.Hour); !got.Equal(time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v", got)
	}
	exact := time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)
	if got := nextSlot(exact, 3*time.Hour); !got.Equal(exact.Add(3 * time.Hour)) {
		t.Fatalf("exact slot: %v", got)
	}
}
