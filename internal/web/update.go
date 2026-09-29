package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if s.Update == nil {
		redirect(w, r, "/settings#updates", errors.New("updates are not available"), "")
		return
	}
	st, err := s.Update.Check(r.Context())
	msg := ""
	switch {
	case err != nil:
		err = fmt.Errorf("checking for updates failed: %w", err)
	case st.Newer:
		msg = fmt.Sprintf("dootd %s is available (you run %s).", st.Latest, st.Current)
	default:
		msg = fmt.Sprintf("You run the newest version (latest release: %s).", st.Latest)
	}
	redirect(w, r, "/settings#updates", err, msg)
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request) {
	if s.Update == nil {
		redirect(w, r, "/settings#updates", errors.New("updates are not available"), "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	s.Log.Info("update requested from the dashboard", "ip", r.Header.Get("CF-Connecting-IP"))
	rel, err := s.Update.Install(ctx)
	if err != nil {
		redirect(w, r, "/settings#updates", fmt.Errorf("update not installed: %w", err), "")
		return
	}
	redirect(w, r, "/settings#updates", nil, fmt.Sprintf(
		"dootd %s was verified and installed. dootd and all apps are restarting now (a few seconds); reload this page in a moment.", rel.Tag))
}
