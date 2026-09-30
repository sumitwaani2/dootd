package deployer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Deployment statuses.
const (
	StatusQueued    = "queued"
	StatusPreparing = "preparing" // downloading, verifying and unpacking the release
	StatusDeploying = "deploying"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Deployment kinds.
const (
	KindDeploy   = "deploy"
	KindRollback = "rollback"
)

// Deployment is one row of the deploy history.
type Deployment struct {
	ID         int64     `json:"id"`
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	ReleaseID  string    `json:"release_id"`
	GitSHA     string    `json:"git_sha"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Done reports whether the deployment has finished.
func (d Deployment) Done() bool { return d.Status == StatusSucceeded || d.Status == StatusFailed }

// Release is a GitHub release unpacked on the server (at most KeepReleases).
type Release struct {
	App        string    `json:"app"`
	ID         string    `json:"id"`      // the tag
	Subject    string    `json:"subject"` // the release name
	SHA256     string    `json:"sha256"`  // of the tarball
	Run        []string  `json:"run"`
	HealthPath string    `json:"health_path"`
	CreatedAt  time.Time `json:"created_at"`
	Current    bool      `json:"current"`
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

const deploymentCols = `id, app, kind, status, release_id, git_sha, error, created_at, started_at, finished_at`

func scanDeployment(s interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var c, st, f int64
	err := s.Scan(&d.ID, &d.App, &d.Kind, &d.Status, &d.ReleaseID, &d.GitSHA, &d.Error, &c, &st, &f)
	d.CreatedAt, d.StartedAt, d.FinishedAt = fromUnix(c), fromUnix(st), fromUnix(f)
	return d, err
}

func (d *Deployer) insertDeployment(ctx context.Context, app, kind, releaseID string) (int64, error) {
	res, err := d.db.Writer().ExecContext(ctx,
		`INSERT INTO deployments (app, kind, status, release_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		app, kind, StatusQueued, releaseID, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("deployer: record deployment: %w", err)
	}
	return res.LastInsertId()
}

func (d *Deployer) setStatus(id int64, status string) {
	col := ""
	if status == StatusPreparing {
		col = ", started_at = " + fmt.Sprint(time.Now().Unix())
	}
	if _, err := d.db.Writer().Exec(`UPDATE deployments SET status = ?`+col+` WHERE id = ?`, status, id); err != nil {
		d.log.Error("update deployment", "id", id, "err", err)
	}
}

func (d *Deployer) setRelease(id int64, releaseID, sha string) {
	if _, err := d.db.Writer().Exec(`UPDATE deployments SET release_id = ?, git_sha = ? WHERE id = ?`, releaseID, sha, id); err != nil {
		d.log.Error("update deployment", "id", id, "err", err)
	}
}

func (d *Deployer) finish(id int64, err error) {
	status, msg := StatusSucceeded, ""
	if err != nil {
		status, msg = StatusFailed, err.Error()
	}
	if _, e := d.db.Writer().Exec(`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, msg, time.Now().Unix(), id); e != nil {
		d.log.Error("finish deployment", "id", id, "err", e)
	}
}

// Deployment returns one deployment.
func (d *Deployer) Deployment(ctx context.Context, id int64) (Deployment, error) {
	dep, err := scanDeployment(d.db.Reader().QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return dep, fmt.Errorf("deployment #%d not found", id)
	}
	return dep, err
}

// Deployments returns the newest deployments of app.
func (d *Deployer) Deployments(ctx context.Context, app string, limit int) ([]Deployment, error) {
	rows, err := d.db.Reader().QueryContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE app = ? ORDER BY id DESC LIMIT ?`, app, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func (d *Deployer) insertRelease(ctx context.Context, r Release) error {
	run, _ := json.Marshal(r.Run)
	_, err := d.db.Writer().ExecContext(ctx, `INSERT INTO releases
		(app, id, git_sha, subject, branch, subdir, zig_version, sha256, run, health_path, created_at)
		VALUES (?, ?, '', ?, '', '', '', ?, ?, ?, ?)`,
		r.App, r.ID, r.Subject, r.SHA256, string(run), r.HealthPath, unix(r.CreatedAt))
	return err
}

func (d *Deployer) deleteRelease(app, id string) error {
	_, err := d.db.Writer().Exec(`DELETE FROM releases WHERE app = ? AND id = ?`, app, id)
	return err
}

// release loads one release row.
func (d *Deployer) release(ctx context.Context, app, id string) (Release, error) {
	rs, err := d.queryReleases(ctx, `WHERE app = ? AND id = ?`, app, id)
	if err != nil {
		return Release{}, err
	}
	if len(rs) == 0 {
		return Release{}, fmt.Errorf("release %s of %s not found (only the last 3 releases are kept)", id, app)
	}
	return rs[0], nil
}

// Releases lists the kept releases of app, newest first.
func (d *Deployer) Releases(ctx context.Context, app string) ([]Release, error) {
	rs, err := d.queryReleases(ctx, `WHERE app = ? ORDER BY created_at DESC, id DESC`, app)
	if err != nil {
		return nil, err
	}
	cur := d.currentRelease(app)
	for i := range rs {
		rs[i].Current = rs[i].ID == cur
	}
	return rs, nil
}

func (d *Deployer) queryReleases(ctx context.Context, where string, args ...any) ([]Release, error) {
	rows, err := d.db.Reader().QueryContext(ctx, `SELECT app, id, subject, sha256, run, health_path, created_at
		FROM releases `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var run string
		var created int64
		if err := rows.Scan(&r.App, &r.ID, &r.Subject, &r.SHA256, &run, &r.HealthPath, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(run), &r.Run); err != nil {
			return nil, fmt.Errorf("release %s: bad run column: %w", r.ID, err)
		}
		r.CreatedAt = fromUnix(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DesiredRunning reports whether app should run after a dootd restart
// (default: yes).
func (d *Deployer) DesiredRunning(ctx context.Context, app string) bool {
	var s string
	err := d.db.Reader().QueryRowContext(ctx, `SELECT desired_state FROM app_state WHERE app = ?`, app).Scan(&s)
	return err != nil || s != "stopped"
}

// SetDesired records whether app should be running.
func (d *Deployer) SetDesired(ctx context.Context, app string, running bool) error {
	s := "stopped"
	if running {
		s = "running"
	}
	_, err := d.db.Writer().ExecContext(ctx, `INSERT INTO app_state (app, desired_state) VALUES (?, ?)
		ON CONFLICT (app) DO UPDATE SET desired_state = excluded.desired_state`, app, s)
	return err
}

// recover marks deployments interrupted by a dootd restart as failed.
func (d *Deployer) recoverInterrupted(ctx context.Context) (int64, error) {
	res, err := d.db.Writer().ExecContext(ctx, `UPDATE deployments
		SET status = ?, error = 'interrupted: dootd restarted during this deployment', finished_at = ?
		WHERE status IN (?, ?, ?, 'building')`, StatusFailed, time.Now().Unix(), StatusQueued, StatusPreparing, StatusDeploying)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
