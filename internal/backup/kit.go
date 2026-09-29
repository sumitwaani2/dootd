package backup

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kit is the content of a recovery kit (Settings → Backups → Download
// recovery kit). It holds what `dootd init --restore` needs to rebuild a
// server from the bucket, except the S3 access key and secret.
type Kit struct {
	Created   time.Time
	Server    string
	HostID    string
	Dashboard string // hostname, without https://
	Version   string
	MasterKey string // 64 hex characters
	KeyPath   string
	// Bucket location ("" when no bucket was configured).
	Endpoint string
	Region   string
	Bucket   string
	Prefix   string
}

// Format renders the kit as the text file the dashboard offers.
func (k Kit) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dootd recovery kit\n==================\n\n")
	fmt.Fprintf(&b, "Created:     %s\nServer:      %s\nHost ID:     %s\nDashboard:   https://%s\ndootd:       %s\n\n",
		k.Created.UTC().Format(time.RFC3339), k.Server, k.HostID, k.Dashboard, k.Version)
	fmt.Fprintf(&b, "Master key (%s, mode 0600):\n%s\n", k.KeyPath, k.MasterKey)
	if k.Bucket != "" {
		fmt.Fprintf(&b, "Backups:     %s  bucket %s  under %s/%s/\n", k.Endpoint, k.Bucket, k.Prefix, k.HostID)
		fmt.Fprintf(&b, "Region:      %s\n", k.Region)
		fmt.Fprintf(&b, "             (the access key is not included; keep it with this kit)\n")
	} else {
		fmt.Fprintf(&b, "Backups:     no S3 bucket configured; backups are only on the server itself\n")
	}
	b.WriteString(`
The master key decrypts the tokens and env vars stored in dootd.db,
including the copies in the daily dootd.db backups (<prefix>/<host id>/_dootd/).
App databases in <prefix>/<host id>/<app>/ are not encrypted by dootd
(protect the bucket itself). Keep this file offline, e.g. in a password
manager. Anyone with it and a dootd.db backup can read your secrets.

To rebuild this server on a new machine:
  curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
  sudo dootd init --restore <this file>
`)
	return b.String()
}

var (
	kitLine    = regexp.MustCompile(`(?m)^([A-Za-z ]+?):[ \t]+(.+?)[ \t]*$`)
	kitKey     = regexp.MustCompile(`(?m)^[0-9a-fA-F]{64}$`)
	kitBackups = regexp.MustCompile(`^(\S+)\s+bucket\s+(\S+)\s+under\s+(\S+)/([0-9a-f]+)/$`)
)

// ParseKit reads a recovery kit written by Format (older kits without a
// Region line are accepted; the region is then empty).
func ParseKit(text string) (Kit, error) {
	var k Kit
	if !strings.HasPrefix(strings.TrimSpace(text), "dootd recovery kit") {
		return k, errors.New("not a dootd recovery kit (it must start with \"dootd recovery kit\")")
	}
	fields := map[string]string{}
	for _, m := range kitLine.FindAllStringSubmatch(text, -1) {
		if _, dup := fields[m[1]]; !dup {
			fields[m[1]] = m[2]
		}
	}
	k.Server, k.HostID, k.Version, k.Region = fields["Server"], fields["Host ID"], fields["dootd"], fields["Region"]
	k.Dashboard = strings.TrimSuffix(strings.TrimPrefix(fields["Dashboard"], "https://"), "/")
	if t, err := time.Parse(time.RFC3339, fields["Created"]); err == nil {
		k.Created = t
	}
	keys := kitKey.FindAllString(text, -1)
	if len(keys) != 1 {
		return k, errors.New("recovery kit: expected exactly one 64-character master key line")
	}
	k.MasterKey = strings.ToLower(keys[0])
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(k.HostID) {
		return k, fmt.Errorf("recovery kit: bad host id %q", k.HostID)
	}
	if b := fields["Backups"]; b != "" && !strings.HasPrefix(b, "no S3 bucket") {
		m := kitBackups.FindStringSubmatch(b)
		if m == nil {
			return k, fmt.Errorf("recovery kit: cannot read the Backups line %q", b)
		}
		if m[4] != k.HostID {
			return k, fmt.Errorf("recovery kit: the Backups line names host %s, not %s", m[4], k.HostID)
		}
		k.Endpoint, k.Bucket, k.Prefix = m[1], m[2], m[3]
	}
	return k, nil
}
