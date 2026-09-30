package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type tarEntry struct {
	typ        byte // d, f, l
	name, link string
}

// parseEntries turns "f a/b\nl a/c ../x\nd a/d" into entries.
func parseEntries(spec string) []tarEntry {
	var es []tarEntry
	for _, line := range strings.Split(spec, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || len(f[0]) != 1 {
			continue
		}
		e := tarEntry{typ: f[0][0], name: f[1]}
		if len(f) > 2 {
			e.link = f[2]
		}
		es = append(es, e)
	}
	return es
}

func writeTarGz(t testing.TB, path string, es []tarEntry) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range es {
		h := &tar.Header{Name: "./" + e.name, Mode: 0o755}
		switch e.typ {
		case 'd':
			h.Typeflag = tar.TypeDir
		case 'l':
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, 2
		}
		if err := tw.WriteHeader(h); err != nil {
			continue
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("hi"))
		}
	}
	tw.Close()
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// unpack runs Unpack with dst nested three levels deep and checks that
// nothing was written outside dst.
func unpack(t testing.TB, es []tarEntry) (string, error) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deep, 0o755)
	archive := filepath.Join(root, "app.tar.gz")
	writeTarGz(t, archive, es)
	dst := filepath.Join(deep, "dst")
	err := Unpack(archive, dst, MaxUnpacked)
	want := map[string][]string{root: {"a", "app.tar.gz"}, filepath.Join(root, "a"): {"b"},
		filepath.Join(root, "a", "b"): {"c"}, deep: {"dst"}}
	for dir, names := range want {
		got, _ := os.ReadDir(dir)
		var have []string
		for _, g := range got {
			have = append(have, g.Name())
		}
		if strings.Join(have, ",") != strings.Join(names, ",") {
			t.Fatalf("extraction wrote outside dst: %s has %v (entries %+v)", dir, have, es)
		}
	}
	return dst, err
}

func TestUnpack(t *testing.T) {
	dst, err := unpack(t, parseEntries("d bin\nf bin/app\nf dootd.toml\nl static/link ../bin/app\nf templates/x.html"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "static", "link")); err != nil || string(b) != "hi" {
		t.Fatalf("link: %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "bin", "app")); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestUnpackRejects(t *testing.T) {
	for name, spec := range map[string]string{
		"dotdot":          "f ../evil",
		"absolute link":   "l x /etc/passwd",
		"escaping link":   "l x ../../y",
		"through symlink": "d a\nl a/b ..\nl a/b/c ..\nf a/b/c/evil",
		"file via link":   "l s .\nf s/../../evil",
		"link to dir":     "d real\nl sub real\nf sub/x",
		"dir over link":   "l d .\nd d",
		"duplicate file":  "f x\nf x",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := unpack(t, parseEntries(spec)); err == nil {
				t.Fatalf("accepted %q", spec)
			}
		})
	}
}

func TestUnpackLimits(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, a, parseEntries("f a\nf b"))
	if err := Unpack(a, filepath.Join(dir, "d"), 3); err == nil || !strings.Contains(err.Error(), "expands to more than") {
		t.Fatalf("size limit: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "plain"), []byte("not gzip"), 0o600)
	if err := Unpack(filepath.Join(dir, "plain"), filepath.Join(dir, "e"), MaxUnpacked); err == nil {
		t.Fatal("non-gzip accepted")
	}
}

// FuzzUnpack builds archives from fuzzed tarEntry lists: extraction may fail,
// but must never write outside its destination.
func FuzzUnpack(f *testing.F) {
	for _, s := range []string{
		"d bin\nf bin/app",
		"d a\nl a/b ..\nl a/b/c ..\nf a/b/c/evil",
		"l x y\nl y ..\nf x/z",
		"l a .\nl a/b ..\nd a/b/c\nf a/b/c/d",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		es := parseEntries(spec)
		if len(es) > 12 {
			return
		}
		unpack(t, es)
	})
}

func TestChecksum(t *testing.T) {
	sums := []byte("0f833f23eb1e8d899fe057240446b8ddb946fd3a375059bebec9864b868e6dcd  app-linux-arm64.tar.gz\n" +
		"A8F5B15B413FA1645E527A8989C5586A7A88E529C10B2A1C295C3BFC43355750 *app-linux-amd64.tar.gz\n")
	if s, err := Checksum(sums, "app-linux-amd64.tar.gz"); err != nil || s != "a8f5b15b413fa1645e527a8989c5586a7a88e529c10b2a1c295c3bfc43355750" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := Checksum(sums, "app-linux-riscv.tar.gz"); err == nil {
		t.Fatal("missing line accepted")
	}
	if _, err := Checksum([]byte("zz  app-linux-amd64.tar.gz"), "app-linux-amd64.tar.gz"); err == nil {
		t.Fatal("bad hash accepted")
	}
}

func TestCheckBinary(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable() // the test binary: an ELF for this machine
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(self)
	os.WriteFile(filepath.Join(root, "app"), b, 0o755)
	os.WriteFile(filepath.Join(root, "noexec"), b, 0o644)
	os.WriteFile(filepath.Join(root, "script"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("/bin/sh", filepath.Join(root, "outside"))
	if err := CheckBinary(root, "app", runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	for rel, want := range map[string]string{"missing": "not in the release", "noexec": "not executable",
		"script": "not a Linux (ELF) binary", "outside": "points outside"} {
		if err := CheckBinary(root, rel, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", rel, err, want)
		}
	}
	if err := CheckBinary(root, "app", other); err == nil || !strings.Contains(err.Error(), "but this server is "+other) {
		t.Errorf("wrong arch: %v", err)
	}
}
