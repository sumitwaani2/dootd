package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.toml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataRoot != DefaultDataRoot || c.MasterKey != DefaultMasterKey || c.Edge.Listen != ":443" || c.Update.Repo != DefaultUpdateRepo {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if !c.Edge.AOPEnabled() {
		t.Fatal("AOP should default to on")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml"), true); err == nil {
		t.Fatal("mustExist should fail for a missing file")
	}
}

func TestLoadRejects(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":    "bogus = 1\n",
		"relative root":  "data_root = \"var/lib\"\n",
		"short interval": "[backups]\ninterval = \"1s\"\n",
		"retention < iv": "[backups]\ninterval = \"1h\"\nretention = \"30m\"\n",
		"bad duration":   "[backups]\ninterval = \"soon\"\n",
	} {
		p := filepath.Join(t.TempDir(), "c.toml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := Load(p, true); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	c, _ := Load("/nonexistent", false)
	c.Edge.DashboardDomain = "dootd.example.com"
	c.Edge.PublicIPv6 = "off"
	c.Backups.Interval.Duration = time.Hour
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, unwanted := range []string{"retention", "data_root", "listen", "repo", "monitoring"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("saved config should not contain default %q:\n%s", unwanted, s)
		}
	}
	c2, err := Load(p, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, s)
	}
	if c2.Edge.DashboardDomain != "dootd.example.com" || c2.Edge.PublicIPv6 != "off" ||
		c2.Backups.Interval.Duration != time.Hour || c2.Edge.Listen != ":443" {
		t.Fatalf("round trip changed values: %+v", c2)
	}
}
