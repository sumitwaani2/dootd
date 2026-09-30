package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/sumitwaani2/dootd/internal/app"
)

// healthCheck waits until the app accepts TCP connections on its port and
// answers GET health_path with 2xx or 3xx, or until the policy timeout.
func healthCheck(ctx context.Context, spec app.Spec, p Policy) error {
	ctx, cancel := context.WithTimeout(ctx, p.HealthTimeout)
	defer cancel()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.Port))
	url := "http://" + addr + spec.HealthPath
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a redirect counts as healthy
		},
		Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil},
	}

	var last error
	deadline, _ := ctx.Deadline()
	t := time.NewTicker(p.HealthInterval)
	defer t.Stop()
	for {
		err := probe(ctx, client, addr, url, spec.Domain)
		if err == nil {
			return nil
		}
		// A probe cut short by the deadline says nothing about the app;
		// keep the previous result (e.g. "GET /healthz returned 404").
		// Compare clocks: the dial can fail at the deadline slightly
		// before ctx.Err() is set.
		if last == nil || time.Now().Before(deadline) {
			last = err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("not healthy within %s: %w", p.HealthTimeout, last)
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

func probe(ctx context.Context, c *http.Client, addr, url, host string) error {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("nothing listening on %s", addr)
		}
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	conn.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if host != "" {
		req.Host = host
	}
	req.Header.Set("User-Agent", "dootd-health")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}
	return nil
}
