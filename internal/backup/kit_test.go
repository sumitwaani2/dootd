package backup

import (
	"strings"
	"testing"
	"time"
)

func TestKitRoundTrip(t *testing.T) {
	k := Kit{
		Created: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), Server: "vps1", HostID: "0123456789ab",
		Dashboard: "dootd.example.com", Version: "v1.0.0", KeyPath: "/etc/dootd/master.key",
		MasterKey: strings.Repeat("ab", 32), Endpoint: "https://acct.r2.cloudflarestorage.com",
		Region: "auto", Bucket: "backups", Prefix: "dootd",
	}
	got, err := ParseKit(k.Format())
	if err != nil {
		t.Fatal(err)
	}
	k.KeyPath = "" // not parsed
	if got != k {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, k)
	}
}

func TestKitWithoutBucket(t *testing.T) {
	k := Kit{HostID: "0123456789ab", Dashboard: "d.example.com", MasterKey: strings.Repeat("0f", 32)}
	got, err := ParseKit(k.Format())
	if err != nil {
		t.Fatal(err)
	}
	if got.Bucket != "" || got.MasterKey != k.MasterKey {
		t.Fatalf("%+v", got)
	}
}

// Kits downloaded before Phase 7 have no Region line.
func TestKitPhase5Format(t *testing.T) {
	text := `dootd recovery kit
==================

Created:     2026-09-27T10:00:00Z
Server:      old
Host ID:     a1b2c3d4e5f6
Dashboard:   https://dootd.example.test
dootd:       v0.6.0

Master key (/etc/dootd/master.key, mode 0600):
` + strings.Repeat("cd", 32) + `
Backups:     http://127.0.0.1:9000  bucket backups  under dootd/a1b2c3d4e5f6/
             (the access key is not included; keep it with this kit)
`
	k, err := ParseKit(text)
	if err != nil {
		t.Fatal(err)
	}
	if k.Endpoint != "http://127.0.0.1:9000" || k.Bucket != "backups" || k.Prefix != "dootd" || k.Region != "" || k.Dashboard != "dootd.example.test" {
		t.Fatalf("%+v", k)
	}
}

func TestKitRejects(t *testing.T) {
	good := Kit{HostID: "0123456789ab", MasterKey: strings.Repeat("ab", 32), Bucket: "b", Endpoint: "https://e", Prefix: "p"}.Format()
	for name, text := range map[string]string{
		"not a kit":      "hello",
		"no key":         strings.Replace(good, strings.Repeat("ab", 32), "", 1),
		"two keys":       good + strings.Repeat("cd", 32) + "\n",
		"bad host id":    strings.Replace(good, "Host ID:     0123456789ab", "Host ID:     xyz", 1),
		"host mismatch":  strings.Replace(good, "under p/0123456789ab/", "under p/ffffffffffff/", 1),
		"garbled bucket": strings.Replace(good, "bucket b", "bucket", 1),
	} {
		if _, err := ParseKit(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// FuzzParseKit: no panic; an accepted kit always has a usable key and host id.
func FuzzParseKit(f *testing.F) {
	f.Add(Kit{HostID: "0123456789ab", MasterKey: strings.Repeat("ab", 32), Bucket: "b", Endpoint: "https://e", Prefix: "p", Region: "auto"}.Format())
	f.Add("dootd recovery kit\nHost ID: 0123456789ab\n" + strings.Repeat("ab", 32) + "\n")
	f.Fuzz(func(t *testing.T, text string) {
		k, err := ParseKit(text)
		if err != nil {
			return
		}
		if len(k.MasterKey) != 64 || len(k.HostID) != 12 {
			t.Fatalf("accepted %+v", k)
		}
		if k.Bucket != "" && (k.Endpoint == "" || k.Prefix == "") {
			t.Fatalf("bucket without location: %+v", k)
		}
	})
}
