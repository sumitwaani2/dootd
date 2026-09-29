package toolchain

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
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

func writeTarXz(t testing.TB, path string, es []tarEntry) {
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
	for _, e := range es {
		h := &tar.Header{Name: "zig-linux/" + e.name, Mode: 0o755}
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
	xw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// extract runs extractTarXz with dst nested three levels deep and checks
// that nothing was written outside dst.
func extract(t testing.TB, es []tarEntry) (string, error) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deep, 0o755)
	archive := filepath.Join(root, "zig.tar.xz")
	writeTarXz(t, archive, es)
	dst := filepath.Join(deep, "dst")
	err := extractTarXz(archive, dst)
	want := map[string][]string{root: {"a", "zig.tar.xz"}, filepath.Join(root, "a"): {"b"},
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

func TestExtract(t *testing.T) {
	dst, err := extract(t, parseEntries("d bin\nf bin/zig\nl lib/link ../bin/zig\nf lib/std/x.zig"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "lib", "link")); err != nil || string(b) != "hi" {
		t.Fatalf("link: %q %v", b, err)
	}
}

func TestExtractRejects(t *testing.T) {
	for name, spec := range map[string]string{
		"dotdot":          "f ../evil",
		"absolute link":   "l x /etc/passwd",
		"escaping link":   "l x ../../y",
		"through symlink": "d a\nl a/b ..\nl a/b/c ..\nf a/b/c/evil",
		"file via link":   "l s .\nf s/../../evil",
		"link to dir":     "d real\nl sub real\nf sub/x",
		"dir over link":   "l d .\nd d/..",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := extract(t, parseEntries(spec)); err == nil && name != "dir over link" {
				t.Fatalf("accepted %q", spec)
			}
		})
	}
}

// FuzzExtract builds archives from fuzzed tarEntry lists: extraction may fail,
// but must never write outside its destination.
func FuzzExtract(f *testing.F) {
	for _, s := range []string{
		"d bin\nf bin/zig",
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
		extract(t, es)
	})
}
