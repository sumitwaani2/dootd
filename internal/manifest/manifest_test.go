package manifest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumitwaani2/dootd/internal/app"
)

func TestParse(t *testing.T) {
	m, err := Parse([]byte(`contract = 1
zig_version = "0.16.0"
run = "zig-out/bin/app --port-from-env"
health_path = "/healthz"
`), app.TypeZig)
	if err != nil {
		t.Fatal(err)
	}
	if m.ZigVersion != "0.16.0" || m.Build != DefaultBuild(app.TypeZig) || len(m.Run) != 2 || m.HealthPath != "/healthz" {
		t.Fatalf("%+v", m)
	}
	m, err = Parse([]byte("contract = 1\nzig_version = \"0.14.1\"\nrun = \"build/app\"\n"), app.TypeC)
	if err != nil || m.Build != "make" || m.HealthPath != app.DefaultHealthPath {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestParseReportsEverything(t *testing.T) {
	_, err := Parse([]byte(`contract = 2
zig_version = "master"
run = "/usr/bin/app"
health_path = "healthz"
build = " "
colour = "blue"
`), app.TypeZig)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"contract = 2", "zig_version", "relative", "health_path", "build must not be empty", `unknown key "colour"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	for _, body := range []string{"", "not toml ===", "contract = 1\nzig_version = \"0.16.0\"\nrun = \"../escape\"\n"} {
		if _, err := Parse([]byte(body), app.TypeZig); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

// FuzzParse: never panics, and anything accepted satisfies the contract.
func FuzzParse(f *testing.F) {
	f.Add([]byte("contract = 1\nzig_version = \"0.16.0\"\nrun = \"zig-out/bin/app\"\n"))
	f.Add([]byte("contract = 1\nzig_version = \"0.16.0\"\nrun = \"a 'b c' \\\"d\\\"\"\nbuild = \"make && make install\"\nhealth_path = \"/h\"\n"))
	f.Add([]byte("contract = 1\nzig_version = \"0.16.0\"\nrun = \"./x/../../y\"\n"))
	f.Add([]byte("[run]\nx = 1\n"))
	f.Add([]byte("contract = 99999999999999999999\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Parse(b, app.TypeZig)
		if err != nil {
			return
		}
		if m.Contract != app.ContractVersion || !zigVersionRe.MatchString(m.ZigVersion) || strings.TrimSpace(m.Build) == "" {
			t.Fatalf("accepted invalid manifest: %+v", m)
		}
		if len(m.Run) == 0 || filepath.IsAbs(m.Run[0]) {
			t.Fatalf("bad run: %q", m.Run)
		}
		if c := filepath.Clean(m.Run[0]); c == ".." || strings.HasPrefix(c, "../") {
			t.Fatalf("run escapes the app root: %q", m.Run)
		}
		if !strings.HasPrefix(m.HealthPath, "/") || strings.ContainsAny(m.HealthPath, " \t\r\n") {
			t.Fatalf("bad health path %q", m.HealthPath)
		}
	})
}
