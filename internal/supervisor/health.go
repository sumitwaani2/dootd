package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
	t := time.NewTicker(p.HealthInterval)
	defer t.Stop()
	for {
		last = probe(ctx, client, addr, url, spec.Domain)
		if last == nil {
			return nil
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
		return fmt.Errorf("nothing listening on %s", addr)
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
