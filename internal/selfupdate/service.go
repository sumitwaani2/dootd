package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Settings keys.
const (
	settingLatest = "update_latest"
	settingResult = "update_result"
)

// Settings is the part of the store the service needs.
type Settings interface {
	GetSetting(ctx context.Context, key string) ([]byte, bool, error)
	SetSetting(ctx context.Context, key string, value []byte) error
}

// Service is what the dashboard and `dootd ctl update` use.
type Service struct {
	*Updater
	Store Settings
	// Busy returns why an update must wait ("" = go ahead), e.g. a running
	// deployment.
	Busy func() string
	// Restart shuts dootd down gracefully; systemd starts the new binary.
	Restart func()
	Log     *slog.Logger

	mu         sync.Mutex
	installing bool
}

// Status is shown on the Settings page.
type Status struct {
	Current    string    `json:"current"`
	Dev        bool      `json:"dev_build"`
	Latest     string    `json:"latest,omitempty"`
	LatestURL  string    `json:"latest_url,omitempty"`
	Checked    time.Time `json:"checked_at"`
	Newer      bool      `json:"newer"`
	CheckError string    `json:"check_error,omitempty"`
	Installing bool      `json:"installing"`
	Last       *Result   `json:"last_update,omitempty"`
}

type latest struct {
	Tag     string    `json:"tag"`
	URL     string    `json:"url"`
	Checked time.Time `json:"checked"`
	Error   string    `json:"error,omitempty"`
}

// Status returns the stored state (no network).
func (s *Service) Status(ctx context.Context) Status {
	st := Status{Current: s.Current, Dev: IsDevBuild(s.Current)}
	if b, ok, _ := s.Store.GetSetting(ctx, settingLatest); ok {
		var l latest
		if json.Unmarshal(b, &l) == nil {
			st.Latest, st.LatestURL, st.Checked, st.CheckError = l.Tag, l.URL, l.Checked, l.Error
			st.Newer = l.Tag != "" && Newer(l.Tag, s.Current)
		}
	}
	if b, ok, _ := s.Store.GetSetting(ctx, settingResult); ok {
		var r Result
		if json.Unmarshal(b, &r) == nil {
			st.Last = &r
		}
	}
	s.mu.Lock()
	st.Installing = s.installing
	s.mu.Unlock()
	return st
}

// Check asks GitHub for the latest release and remembers the answer.
func (s *Service) Check(ctx context.Context) (Status, error) {
	rel, err := s.Latest(ctx)
	l := latest{Tag: rel.Tag, URL: rel.URL, Checked: time.Now().UTC()}
	if err != nil {
		l.Tag, l.Error = "", err.Error()
	}
	b, _ := json.Marshal(l)
	s.Store.SetSetting(ctx, settingLatest, b)
	return s.Status(ctx), err
}

// Install checks again, applies the latest release when it is newer and
// restarts dootd shortly after (so the caller can still answer).
func (s *Service) Install(ctx context.Context) (Release, error) {
	s.mu.Lock()
	if s.installing {
		s.mu.Unlock()
		return Release{}, errors.New("an update is already being installed")
	}
	s.installing = true
	s.mu.Unlock()
	done := func() { s.mu.Lock(); s.installing = false; s.mu.Unlock() }

	if s.Busy != nil {
		if why := s.Busy(); why != "" {
			done()
			return Release{}, fmt.Errorf("not now: %s", why)
		}
	}
	rel, err := s.Latest(ctx)
	if err != nil {
		done()
		return rel, err
	}
	if !Newer(rel.Tag, s.Current) {
		done()
		return rel, fmt.Errorf("%s is already the newest version (latest release: %s)", s.Current, rel.Tag)
	}
	s.Log.Info("installing update", "from", s.Current, "to", rel.Tag)
	if err := s.Apply(ctx, rel); err != nil {
		done()
		s.Log.Error("update failed; nothing was changed", "to", rel.Tag, "err", err)
		return rel, err
	}
	s.Log.Info("update installed; restarting", "to", rel.Tag)
	time.AfterFunc(time.Second, s.Restart)
	return rel, nil
}

// FinishAfter records the outcome of an update once this binary has run
// for delay (a rolled-back update is recorded at once). Call it at startup.
func (s *Service) FinishAfter(ctx context.Context, delay time.Duration) {
	m, ok, err := ReadMarker(s.DataRoot)
	if err != nil {
		s.Log.Warn("update marker", "err", err)
		return
	}
	if !ok {
		return
	}
	finish := func() {
		r, ok, err := Finish(s.DataRoot, s.Current)
		if err != nil {
			s.Log.Warn("update marker", "err", err)
		}
		if !ok {
			return
		}
		b, _ := json.Marshal(r)
		s.Store.SetSetting(context.WithoutCancel(ctx), settingResult, b)
		if r.OK {
			s.Log.Info("update finished", "from", r.From, "to", r.To)
		} else {
			s.Log.Error("update rolled back", "detail", r.Detail)
		}
	}
	if m.Status != StatusPending {
		finish()
		return
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(delay):
			finish()
		}
	}()
}
