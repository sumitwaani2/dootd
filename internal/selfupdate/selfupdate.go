// Package selfupdate replaces the dootd binary with a newer GitHub release
// and rolls back when the new binary does not start (docs/architecture.md
// §15, Req 17).
//
// Flow:
//  1. Latest asks the GitHub Releases API for the newest release.
//  2. Apply downloads dootd-linux-<arch> and checksums.txt, verifies the
//     SHA-256, runs `<new> version` as a sanity check, keeps the running
//     binary as dootd.prev, renames the new one into place and writes the
//     update marker (<data root>/update.json). The caller then exits and
//     systemd starts the new binary.
//  3. The new binary copies dootd.db to dootd.db.pre-update before it
//     applies any migration (store.PendingMigrations + serve).
//  4. Before every start, systemd runs `dootd.prev update-guard` (Guard):
//     it counts the starts of a pending update, and after 3 failed ones
//     puts dootd.prev and dootd.db.pre-update back.
//  5. Once the new binary has run for a while, Finish marks the update done.
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Files next to the binary and in the data root.
const (
	PrevSuffix      = ".prev"
	newSuffix       = ".new"
	MarkerName      = "update.json"
	PreUpdateSuffix = ".pre-update"
	// MaxAttempts is how many times the new binary may fail to start.
	MaxAttempts = 3
)

// Statuses of the marker.
const (
	StatusPending    = "pending"
	StatusRolledBack = "rolled_back"
)

// Release is one GitHub release.
type Release struct {
	Tag       string            `json:"tag"`
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Published time.Time         `json:"published"`
	Assets    map[string]string `json:"assets"` // name -> download URL
}

// Updater checks for and installs releases.
type Updater struct {
	Repo     string // owner/name
	API      string // default https://api.github.com
	Current  string // running version (buildinfo.Version)
	Binary   string // path of the running binary (default: os.Executable)
	DataRoot string // holds update.json and dootd.db
	HTTP     *http.Client
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (u *Updater) api() string {
	if u.API != "" {
		return strings.TrimRight(u.API, "/")
	}
	return "https://api.github.com"
}

func (u *Updater) binary() (string, error) {
	if u.Binary != "" {
		return u.Binary, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// Latest returns the newest published (non-prerelease) release.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.api()+"/repos/"+u.Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dootd/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("selfupdate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Release{}, fmt.Errorf("selfupdate: no published release in %s", u.Repo)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Release{}, fmt.Errorf("selfupdate: GitHub API: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var r struct {
		Tag       string    `json:"tag_name"`
		Name      string    `json:"name"`
		URL       string    `json:"html_url"`
		Published time.Time `json:"published_at"`
		Assets    []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("selfupdate: decode release: %w", err)
	}
	rel := Release{Tag: r.Tag, Name: r.Name, URL: r.URL, Published: r.Published, Assets: map[string]string{}}
	for _, a := range r.Assets {
		rel.Assets[a.Name] = a.URL
	}
	if _, ok := parseVersion(rel.Tag); !ok {
		return rel, fmt.Errorf("selfupdate: latest release tag %q is not a version like v1.2.3", rel.Tag)
	}
	return rel, nil
}

// AssetName is the binary for this machine.
func AssetName() string { return "dootd-linux-" + runtime.GOARCH }

// ---------------------------------------------------------------- versions

type version struct {
	major, minor, patch int
	pre                 string // "" for a release
	ahead               int    // git describe: commits after the tag
}

// parseVersion reads v1.2.3, v1.2.3-rc1 and git describe output such as
// v1.2.3-4-gabcdef0(-dirty).
func parseVersion(s string) (version, bool) {
	var v version
	s, ok := strings.CutPrefix(strings.TrimSpace(s), "v")
	if !ok {
		return v, false
	}
	s = strings.TrimSuffix(s, "-dirty")
	core, rest, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	if rest != "" {
		// git describe: <n>-g<hash>, possibly after a prerelease part.
		fs := strings.Split(rest, "-")
		if l := len(fs); l >= 2 && strings.HasPrefix(fs[l-1], "g") {
			if n, err := strconv.Atoi(fs[l-2]); err == nil {
				v.ahead = n
				fs = fs[:l-2]
			}
		}
		v.pre = strings.Join(fs, "-")
	}
	return v, true
}

func cmpVersion(a, b version) int {
	for _, d := range []int{a.major - b.major, a.minor - b.minor, a.patch - b.patch} {
		if d != 0 {
			return d
		}
	}
	switch {
	case a.pre == b.pre:
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	case a.pre < b.pre:
		return -1
	default:
		return 1
	}
	return a.ahead - b.ahead
}

// Newer reports whether release tag is newer than the running version. A
// development build (no version) is always offered the release.
func Newer(tag, current string) bool {
	t, ok := parseVersion(tag)
	if !ok {
		return false
	}
	c, ok := parseVersion(current)
	if !ok {
		return true
	}
	return cmpVersion(t, c) > 0
}

// IsDevBuild reports whether current is not a release version.
func IsDevBuild(current string) bool {
	v, ok := parseVersion(current)
	return !ok || v.ahead > 0 || strings.HasSuffix(current, "-dirty")
}

// ---------------------------------------------------------------- apply

// Marker is update.json in the data root.
type Marker struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	Started  time.Time `json:"started"`
	Attempts int       `json:"attempts"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
}

// MarkerPath is where the marker lives.
func MarkerPath(dataRoot string) string { return filepath.Join(dataRoot, MarkerName) }

// ReadMarker returns the marker (ok=false if there is none).
func ReadMarker(dataRoot string) (Marker, bool, error) {
	var m Marker
	b, err := os.ReadFile(MarkerPath(dataRoot))
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("selfupdate: %s: %w", MarkerPath(dataRoot), err)
	}
	return m, true, nil
}

func writeMarker(dataRoot string, m Marker) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return writeFileSync(MarkerPath(dataRoot), b, 0o600)
}

// Apply downloads, verifies and installs rel. It does not restart.
func (u *Updater) Apply(ctx context.Context, rel Release) error {
	bin, err := u.binary()
	if err != nil {
		return err
	}
	name := AssetName()
	binURL, sumURL := rel.Assets[name], rel.Assets["checksums.txt"]
	if binURL == "" || sumURL == "" {
		return fmt.Errorf("selfupdate: release %s has no %s or checksums.txt", rel.Tag, name)
	}
	sums, err := u.fetch(ctx, sumURL, 1<<20)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("selfupdate: checksums.txt of %s has no line for %s", rel.Tag, name)
	}
	body, err := u.fetch(ctx, binURL, 200<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("selfupdate: checksum mismatch for %s %s (expected %s, got %s); nothing was changed", name, rel.Tag, want, got)
	}
	newPath := bin + newSuffix
	os.Remove(newPath)
	if err := writeFileSync(newPath, body, 0o755); err != nil {
		return err
	}
	cleanup := func(err error) error { os.Remove(newPath); return err }
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, newPath, "version").Output()
	if err != nil {
		return cleanup(fmt.Errorf("selfupdate: the new binary does not run here: %w", err))
	}
	if !bytes.Contains(out, []byte(" "+rel.Tag+" ")) {
		return cleanup(fmt.Errorf("selfupdate: the new binary reports %q, expected %s", strings.TrimSpace(string(out)), rel.Tag))
	}

	// Keep the running binary as dootd.prev (a hard link keeps its inode
	// once the new file replaces the name).
	prev := bin + PrevSuffix
	tmpPrev := prev + ".tmp"
	os.Remove(tmpPrev)
	if err := os.Link(bin, tmpPrev); err != nil {
		if err := copyFile(bin, tmpPrev, 0o755); err != nil {
			return cleanup(err)
		}
	}
	if err := os.Rename(tmpPrev, prev); err != nil {
		return cleanup(err)
	}
	// A stale database copy from an earlier update must not be restored.
	os.Remove(filepath.Join(u.DataRoot, "dootd.db"+PreUpdateSuffix))
	if err := writeMarker(u.DataRoot, Marker{From: u.Current, To: rel.Tag, Started: time.Now().UTC(), Status: StatusPending}); err != nil {
		return cleanup(err)
	}
	if err := os.Rename(newPath, bin); err != nil {
		os.Remove(MarkerPath(u.DataRoot))
		return cleanup(err)
	}
	return syncDir(filepath.Dir(bin))
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dootd/"+u.Current)
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: download: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("selfupdate: %s is larger than %d bytes", url, limit)
	}
	return b, nil
}

// ---------------------------------------------------------------- guard

// Guard runs before every start (from dootd.prev, the last binary known to
// work). While an update is pending it counts starts; after MaxAttempts
// failed starts it restores dootd.prev and the pre-update database, so the
// next start runs the previous version on a schema it understands.
// It returns a one-line description of what it did ("" = nothing).
func Guard(binary, dataRoot string) (string, error) {
	m, ok, err := ReadMarker(dataRoot)
	if err != nil || !ok || m.Status != StatusPending {
		return "", err
	}
	m.Attempts++
	if m.Attempts <= MaxAttempts {
		if err := writeMarker(dataRoot, m); err != nil {
			return "", err
		}
		return fmt.Sprintf("update %s -> %s: start %d of %d", m.From, m.To, m.Attempts, MaxAttempts), nil
	}
	prev := binary + PrevSuffix
	tmp := binary + ".rollback"
	os.Remove(tmp)
	if err := copyFile(prev, tmp, 0o755); err != nil {
		return "", fmt.Errorf("selfupdate: restore %s: %w", prev, err)
	}
	if err := os.Rename(tmp, binary); err != nil {
		return "", err
	}
	syncDir(filepath.Dir(binary))
	db := filepath.Join(dataRoot, "dootd.db")
	restoredDB := false
	if _, err := os.Stat(db + PreUpdateSuffix); err == nil {
		for _, s := range []string{"-wal", "-shm", "-journal"} {
			os.Remove(db + s)
		}
		if err := os.Rename(db+PreUpdateSuffix, db); err != nil {
			return "", fmt.Errorf("selfupdate: restore %s: %w", db+PreUpdateSuffix, err)
		}
		restoredDB = true
	}
	m.Status = StatusRolledBack
	m.Error = fmt.Sprintf("%s did not start %d times; dootd went back to %s", m.To, MaxAttempts, m.From)
	if restoredDB {
		m.Error += " and the database copy taken before the update"
	}
	if err := writeMarker(dataRoot, m); err != nil {
		return "", err
	}
	return m.Error, nil
}

// Result is the outcome of the last update, for the dashboard.
type Result struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Detail string    `json:"detail"`
}

// Finish is called by a running dootd of version current. It returns the
// outcome of a finished update (ok=false if there was none to report) and
// removes the marker. A pending update of another version is left alone.
func Finish(dataRoot, current string) (Result, bool, error) {
	m, ok, err := ReadMarker(dataRoot)
	if err != nil || !ok {
		return Result{}, false, err
	}
	var r Result
	switch {
	case m.Status == StatusRolledBack:
		r = Result{From: m.From, To: m.To, At: time.Now().UTC(), Detail: m.Error}
	case m.Status == StatusPending && m.To == current:
		r = Result{From: m.From, To: m.To, At: time.Now().UTC(), OK: true,
			Detail: fmt.Sprintf("updated from %s to %s", m.From, m.To)}
	default:
		return Result{}, false, nil
	}
	return r, true, os.Remove(MarkerPath(dataRoot))
}

// ---------------------------------------------------------------- files

func writeFileSync(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileSync(dst, b, mode)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
