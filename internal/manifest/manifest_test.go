package manifest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumitwaani2/dootd/internal/app"
)

func TestParse(t *testing.T) {
	m, err := Parse([]byte(`contract = 2
run = "zig-out/bin/app --port-from-env"
health_path = "/healthz"
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Contract != 2 || len(m.Run) != 2 || m.HealthPath != "/healthz" {
		t.Fatalf("%+v", m)
	}
	m, err = Parse([]byte("contract = 2\nrun = \"build/app\"\n"))
	if err != nil || m.HealthPath != app.DefaultHealthPath {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestParseReportsEverything(t *testing.T) {
	_, err := Parse([]byte(`contract = 3
run = "/usr/bin/app"
health_path = "healthz"
colour = "blue"
`))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"contract = 3", "relative", "health_path", `unknown key "colour"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	for _, body := range []string{"", "not toml ===", "contract = 2\nrun = \"../escape\"\n"} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

// Contract 1 manifests get an explanation, not just "unknown key".
func TestParseContract1(t *testing.T) {
	_, err := Parse([]byte("contract = 1\nzig_version = \"0.16.0\"\nbuild = \"make\"\nrun = \"build/app\"\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"contract = 1 (dootd builds the app) is no longer supported", "zig_version is not used any more", "build is not used any more"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// FuzzParse: never panics, and anything accepted satisfies the contract.
func FuzzParse(f *testing.F) {
	f.Add([]byte("contract = 2\nrun = \"zig-out/bin/app\"\n"))
	f.Add([]byte("contract = 2\nrun = \"a 'b c' \\\"d\\\"\"\nhealth_path = \"/h\"\n"))
	f.Add([]byte("contract = 2\nrun = \"./x/../../y\"\n"))
	f.Add([]byte("[run]\nx = 1\n"))
	f.Add([]byte("contract = 99999999999999999999\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Parse(b)
		if err != nil {
			return
		}
		if m.Contract != app.ContractVersion {
			t.Fatalf("accepted invalid manifest: %+v", m)
		}
		if len(m.Run) == 0 || filepath.IsAbs(m.Run[0]) {
			t.Fatalf("bad run: %q", m.Run)
		}
		if c := filepath.Clean(m.Run[0]); c == ".." || strings.HasPrefix(c, "../") {
			t.Fatalf("run escapes the release: %q", m.Run)
		}
		if !strings.HasPrefix(m.HealthPath, "/") || strings.ContainsAny(m.HealthPath, " \t\r\n") {
			t.Fatalf("bad health path %q", m.HealthPath)
		}
	})
}
