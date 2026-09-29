package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/s3"
)

// policyText describes the backup schedule for pages.
func (s *Server) policyText() string {
	iv, rt := s.Backups.Policy()
	return fmt.Sprintf("every %s and before each deploy, kept %s", shortDur(iv), shortDur(rt))
}

// shortDur renders 3h0m0s as "3h", 1h30m0s as "1h30m", 20s as "20s".
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	b, err := s.Backups.Run(ctx, name, backup.KindManual, true, nil)
	if errors.Is(err, backup.ErrNoDatabases) {
		redirect(w, r, "/apps/"+name+"#backups", errors.New("nothing to back up: the app has no SQLite databases in its DATA_DIR yet"), "")
		return
	}
	msg := fmt.Sprintf("Backup #%d taken (%d database(s)).", b.ID, b.Files)
	switch {
	case err != nil:
	case b.Uploaded():
		msg += " Uploaded to the bucket."
	case b.Error != "":
		err = fmt.Errorf("backup #%d was taken but not uploaded: %s", b.ID, b.Error)
	default:
		msg += " Kept on this server only (no bucket configured)."
	}
	redirect(w, r, "/apps/"+name+"#backups", err, msg)
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		redirect(w, r, "/apps/"+name+"#backups", errors.New("bad backup id"), "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	res, err := s.Backups.Restore(ctx, name, id, nil)
	if err != nil {
		redirect(w, r, "/apps/"+name+"#backups", fmt.Errorf("restore failed, nothing was changed: %w", err), "")
		return
	}
	msg := fmt.Sprintf("Backup #%d restored (%s).", id, strings.Join(res.Files, ", "))
	if res.MovedTo != "" {
		msg += " The previous databases were moved to " + res.MovedTo + "."
	}
	if res.StartError != "" {
		err = fmt.Errorf("%s But the app did not start again: %s", msg, res.StartError)
	} else if res.Restarted {
		msg += " The app was restarted."
	}
	redirect(w, r, "/apps/"+name+"#backups", err, msg)
}

func (s *Server) setS3(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.PostFormValue("remove") == "1" {
		err := s.Backups.RemoveS3Config(ctx)
		redirect(w, r, "/settings#backups", err, "S3 settings removed. Backups are kept on this server only.")
		return
	}
	c := s3.Config{
		Endpoint: r.PostFormValue("endpoint"), Region: r.PostFormValue("region"), Bucket: r.PostFormValue("bucket"),
		AccessKey: r.PostFormValue("access_key"), SecretKey: r.PostFormValue("secret_key"),
	}
	if err := s.Backups.SetS3Config(ctx, c); err != nil {
		redirect(w, r, "/settings#backups", fmt.Errorf("S3 settings not saved: %w", err), "")
		return
	}
	go s.Backups.UploadPending(context.WithoutCancel(ctx))
	redirect(w, r, "/settings#backups", nil, "S3 settings saved (encrypted). A test file was uploaded, read back and deleted. Backups taken so far are being uploaded.")
}
