// Package supervisor runs app processes: one goroutine ("actor") per app
// owns its process, state machine, health checks and restart policy.
//
// States: stopped → starting → running → stopping → stopped, plus
// backoff (waiting to restart after a failure) and crashed (gave up after
// too many failures; needs a manual start).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sumitwaani2/dootd/internal/app"
	"github.com/sumitwaani2/dootd/internal/cgroup"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/logs"
	"github.com/sumitwaani2/dootd/internal/users"
)

// Deps are the supervisor's collaborators.
type Deps struct {
	Layout  layout.Layout
	Cgroups *cgroup.Manager
	Log     *slog.Logger
	Policy  Policy // zero value = DefaultPolicy
}

// Supervisor owns all app actors.
type Supervisor struct {
	deps Deps
	mu   sync.Mutex
	apps map[string]*App
	// order preserves insertion order for sequential boot starts.
	order []string
}

// New creates a Supervisor.
func New(d Deps) *Supervisor {
	if d.Policy == (Policy{}) {
		d.Policy = DefaultPolicy
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if missing := d.Cgroups.Missing(); len(missing) > 0 {
		d.Log.Warn("cgroup controllers unavailable; the matching limits are NOT enforced", "missing", missing)
	}
	return &Supervisor{deps: d, apps: map[string]*App{}}
}

// Add prepares an app (system user, directories, cgroup + limits, log) and
// starts its actor. The app stays stopped until Start is called.
func (s *Supervisor) Add(spec app.Spec) (*App, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[spec.Name]; ok {
		return nil, fmt.Errorf("supervisor: app %q already exists", spec.Name)
	}
	for _, other := range s.apps {
		if other.Spec().Port == spec.Port {
			return nil, fmt.Errorf("supervisor: port %d already used by %q", spec.Port, other.Spec().Name)
		}
	}

	u, err := users.Ensure(spec.Name)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Layout.EnsureApp(spec.Name, u); err != nil {
		return nil, err
	}
	g, err := s.deps.Cgroups.Group(cgroup.Apps, spec.Name)
	if err != nil {
		return nil, err
	}
	if err := applyLimits(g, spec.Limits); err != nil {
		return nil, err
	}
	lg, err := logs.Open(s.deps.Layout.AppLog(spec.Name), logs.Options{})
	if err != nil {
		return nil, err
	}

	a := newApp(spec, s.deps.Layout, u, g, lg, s.deps.Policy, s.deps.Log.With("app", spec.Name))
	s.apps[spec.Name] = a
	s.order = append(s.order, spec.Name)
	go a.loop()
	return a, nil
}

func applyLimits(g *cgroup.Group, l app.Limits) error {
	_, err := g.SetLimits(cgroup.Limits{
		MemoryMax:  l.MemoryMax,
		MemoryHigh: l.MemoryMax / 10 * 9,
		NoSwap:     true, // predictable OOM at memory.max instead of swapping
		CPUMax:     l.CPUMax,
		PidsMax:    l.PidsMax,
	})
	return err
}

// Get returns the app or nil.
func (s *Supervisor) Get(name string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apps[name]
}

// Apps returns all apps in insertion order.
func (s *Supervisor) Apps() []*App {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*App, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.apps[n])
	}
	return out
}

// StartAll starts the given apps one at a time (Req 10.7), waiting for each
// to become healthy or fail before starting the next. Failures are logged;
// failed apps keep retrying under the restart policy.
func (s *Supervisor) StartAll(ctx context.Context) {
	for _, a := range s.Apps() {
		if ctx.Err() != nil {
			return
		}
		if err := a.Start(ctx); err != nil {
			s.deps.Log.Error("app failed to start", "app", a.Spec().Name, "err", err)
		}
	}
}

// StopAll stops every app in parallel and shuts down the actors.
func (s *Supervisor) StopAll(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, a := range s.Apps() {
		wg.Add(1)
		go func(a *App) {
			defer wg.Done()
			if err := a.Stop(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", a.Spec().Name, err))
				mu.Unlock()
			}
			a.close()
		}(a)
	}
	wg.Wait()
	return errors.Join(errs...)
}
